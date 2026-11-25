//go:build integration

package ldapauth

import (
	"context"
	"os"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"github.com/aisa-it/syngo/internal/config"
)

// Настоящий OpenLDAP. Без SYNGOD_TEST_LDAP_URI тест пропускается; стенд:
//
//	docker run -d --name syngo-ldap -p 127.0.0.1:38389:389 -e LDAP_DOMAIN=example.test \
//	    -e LDAP_ADMIN_PASSWORD=test-admin -e LDAP_TLS=false osixia/openldap:1.5.0
//	SYNGOD_TEST_LDAP_URI=ldap://127.0.0.1:38389 go test -tags integration ./internal/ldapauth
const (
	testBase      = "dc=example,dc=test"
	testAdminDN   = "cn=admin,dc=example,dc=test"
	testAdminPass = "test-admin"
	testPeople    = "ou=people,dc=example,dc=test"
)

func ldapURI(t *testing.T) string {
	t.Helper()

	uri := os.Getenv("SYNGOD_TEST_LDAP_URI")
	if uri == "" {
		t.Skip("SYNGOD_TEST_LDAP_URI не задан")
	}

	return uri
}

// seedDirectory создает ou=people и пользователей (тестовые пароли).
func seedDirectory(t *testing.T, uri string) {
	t.Helper()

	conn, err := ldap.DialURL(uri)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	defer conn.Close()

	if err := conn.Bind(testAdminDN, testAdminPass); err != nil {
		t.Fatalf("admin bind: %v", err)
	}

	del := func(dn string) { _ = conn.Del(ldap.NewDelRequest(dn, nil)) }

	for _, uid := range []string{"ivanov", "noposix"} {
		del("uid=" + uid + "," + testPeople)
	}

	del(testPeople)

	add := func(dn string, attrs map[string][]string) {
		req := ldap.NewAddRequest(dn, nil)
		for name, values := range attrs {
			req.Attribute(name, values)
		}

		if err := conn.Add(req); err != nil {
			t.Fatalf("add %s: %v", dn, err)
		}
	}

	add(testPeople, map[string][]string{"objectClass": {"organizationalUnit"}, "ou": {"people"}})
	add("uid=ivanov,"+testPeople, map[string][]string{
		"objectClass": {"inetOrgPerson", "posixAccount"}, "uid": {"ivanov"}, "cn": {"Иванов Иван"},
		"sn": {"Иванов"}, "mail": {"ivanov@example.test"}, "userPassword": {"ivanov-pass"},
		"uidNumber": {"10001"}, "gidNumber": {"10001"}, "homeDirectory": {"/home/ivanov"},
	})
	add("uid=noposix,"+testPeople, map[string][]string{
		"objectClass": {"inetOrgPerson"}, "uid": {"noposix"}, "cn": {"Без posix"}, "sn": {"X"},
		"userPassword": {"noposix-pass"},
	})

	t.Cleanup(func() {
		cleanup, err := ldap.DialURL(uri)
		if err != nil {
			return
		}

		defer cleanup.Close()

		if cleanup.Bind(testAdminDN, testAdminPass) == nil {
			for _, uid := range []string{"ivanov", "noposix"} {
				_ = cleanup.Del(ldap.NewDelRequest("uid="+uid+","+testPeople, nil))
			}

			_ = cleanup.Del(ldap.NewDelRequest(testPeople, nil))
		}
	})
}

// TestOpenLDAP - режимы search и simple synapse-ldap3 против настоящего slapd.
func TestOpenLDAP(t *testing.T) {
	uri := ldapURI(t)
	seedDirectory(t, uri)

	ctx := context.Background()

	search, err := New(config.LDAPProvider{
		URI: config.StringList{"ldap://127.0.0.1:1", uri}, Base: testBase,
		Attributes: config.LDAPAttributes{UID: "uid", Mail: "mail", Name: "cn"},
		BindDN:     testAdminDN, BindPassword: testAdminPass, Filter: "(objectClass=posixAccount)",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, ok, err := search.CheckPassword(ctx, "Ivanov", "ivanov-pass")
	if err != nil || !ok || identity.Localpart != "ivanov" || identity.DisplayName != "Иванов Иван" ||
		identity.Email != "ivanov@example.test" {
		t.Fatalf("search: %+v, %v, %v", identity, ok, err)
	}

	for _, tc := range []struct{ user, password string }{
		{"ivanov", "wrong"}, {"nobody", "x"}, {"noposix", "noposix-pass"}, {"*", "ivanov-pass"},
	} {
		if _, ok, err := search.CheckPassword(ctx, tc.user, tc.password); ok || err != nil {
			t.Errorf("search %s: %v, %v; ожидали отказ", tc.user, ok, err)
		}
	}

	simple, err := New(config.LDAPProvider{
		URI: config.StringList{uri}, Base: testPeople,
		Attributes: config.LDAPAttributes{UID: "uid", Mail: "mail", Name: "cn"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	identity, ok, err = simple.CheckPassword(ctx, "noposix", "noposix-pass")
	if err != nil || !ok || identity.DisplayName != "Без posix" {
		t.Fatalf("simple: %+v, %v, %v", identity, ok, err)
	}

	if _, ok, err := simple.CheckPassword(ctx, "noposix", "wrong"); ok || err != nil {
		t.Fatalf("simple неверный пароль: %v, %v", ok, err)
	}
}
