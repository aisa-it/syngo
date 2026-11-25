package ldapauth

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-ldap/ldap/v3"

	"github.com/aisa-it/syngo/internal/config"
)

// fakeDirectory - подставной каталог: DN -> пароль и атрибуты.
type fakeDirectory struct {
	passwords map[string]string
	entries   map[string]map[string]string
	// down - серверы, к которым нельзя подключиться.
	down     map[string]bool
	dialed   []string
	binds    []string
	filters  []string
	startTLS int
}

type fakeConn struct{ dir *fakeDirectory }

func (c *fakeConn) StartTLS(*tls.Config) error {
	c.dir.startTLS++

	return nil
}

func (c *fakeConn) Bind(dn, password string) error {
	c.dir.binds = append(c.dir.binds, dn)

	want, ok := c.dir.passwords[dn]
	if !ok {
		return ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object"))
	}

	if want != password {
		return ldap.NewError(ldap.LDAPResultInvalidCredentials, errors.New("invalid credentials"))
	}

	return nil
}

var uidInFilter = regexp.MustCompile(`\((\w+)=([^)]*)\)`)

func (c *fakeConn) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	c.dir.filters = append(c.dir.filters, req.Filter)

	if req.Scope == ldap.ScopeBaseObject {
		attrs, ok := c.dir.entries[req.BaseDN]
		if !ok {
			return nil, ldap.NewError(ldap.LDAPResultNoSuchObject, errors.New("no such object"))
		}

		return &ldap.SearchResult{Entries: []*ldap.Entry{ldap.NewEntry(req.BaseDN, toMulti(attrs))}}, nil
	}

	// Все условия (attr=value) фильтра (&...) должны совпасть.
	conditions := uidInFilter.FindAllStringSubmatch(req.Filter, -1)
	result := &ldap.SearchResult{}

	for dn, attrs := range c.dir.entries {
		matched := len(conditions) > 0 && strings.HasSuffix(dn, req.BaseDN)
		for _, cond := range conditions {
			matched = matched && strings.EqualFold(attrs[cond[1]], cond[2])
		}

		if matched {
			result.Entries = append(result.Entries, ldap.NewEntry(dn, toMulti(attrs)))
		}
	}

	return result, nil
}

func (c *fakeConn) Close() error { return nil }

func toMulti(attrs map[string]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range attrs {
		out[k] = []string{v}
	}

	return out
}

func (d *fakeDirectory) dial(_ context.Context, uri string, _ *tls.Config, _ time.Duration) (Conn, error) {
	d.dialed = append(d.dialed, uri)

	if d.down[uri] {
		return nil, errors.New("connection refused")
	}

	return &fakeConn{dir: d}, nil
}

func newDirectory() *fakeDirectory {
	return &fakeDirectory{
		passwords: map[string]string{
			"cn=reader,dc=example,dc=org":                  "reader-secret",
			"uid=ivanov,ou=people,dc=example,dc=org":       "ivanov-pass",
			"uid=petrov,ou=people,dc=example,dc=org":       "petrov-pass",
			"uid=dup,ou=people,dc=example,dc=org":          "dup-pass",
			"uid=dup,ou=staff,ou=people,dc=example,dc=org": "dup-pass",
		},
		entries: map[string]map[string]string{
			"uid=ivanov,ou=people,dc=example,dc=org": {
				"uid": "ivanov", "cn": "Иванов Иван", "mail": "ivanov@example.org", "objectClass": "posixAccount",
			},
			"uid=petrov,ou=people,dc=example,dc=org":       {"uid": "petrov", "cn": "Петров Петр"},
			"uid=dup,ou=people,dc=example,dc=org":          {"uid": "dup", "objectClass": "posixAccount"},
			"uid=dup,ou=staff,ou=people,dc=example,dc=org": {"uid": "dup", "objectClass": "posixAccount"},
		},
		down: map[string]bool{},
	}
}

func searchConfig() config.LDAPProvider {
	return config.LDAPProvider{
		URI:        config.StringList{"ldap://ldap1.example.org:389"},
		Base:       "ou=people,dc=example,dc=org",
		Attributes: config.LDAPAttributes{UID: "uid", Mail: "mail", Name: "cn"},
		BindDN:     "cn=reader,dc=example,dc=org", BindPassword: "reader-secret",
		Filter: "(objectClass=posixAccount)",
	}
}

// TestSearchMode - режим search как в synapse-ldap3: служебный bind, поиск с
// фильтром, bind найденным DN; имя и почта - из атрибутов.
func TestSearchMode(t *testing.T) {
	dir := newDirectory()

	provider, err := New(searchConfig(), dir.dial)
	if err != nil {
		t.Fatal(err)
	}

	identity, ok, err := provider.CheckPassword(context.Background(), "Ivanov", "ivanov-pass")
	if err != nil || !ok {
		t.Fatalf("CheckPassword = %v, %v", ok, err)
	}

	if identity.Localpart != "ivanov" || identity.DisplayName != "Иванов Иван" || identity.Email != "ivanov@example.org" {
		t.Fatalf("identity = %+v", identity)
	}

	if got := dir.filters[0]; got != "(&(uid=Ivanov)(objectClass=posixAccount))" {
		t.Fatalf("фильтр = %q", got)
	}

	if len(dir.binds) != 2 || dir.binds[0] != "cn=reader,dc=example,dc=org" || dir.binds[1] != "uid=ivanov,ou=people,dc=example,dc=org" {
		t.Fatalf("binds = %v", dir.binds)
	}

	// Неверный пароль, нет в каталоге, не проходит фильтр, неоднозначное имя - отказ без ошибки.
	for _, tc := range []struct{ user, password string }{
		{"ivanov", "wrong"}, {"nobody", "x"}, {"petrov", "petrov-pass"}, {"dup", "dup-pass"}, {"ivanov", ""},
	} {
		if _, ok, err := provider.CheckPassword(context.Background(), tc.user, tc.password); ok || err != nil {
			t.Errorf("CheckPassword(%s, %s) = %v, %v; ожидали отказ", tc.user, tc.password, ok, err)
		}
	}
}

// TestSearchModeEscapesFilter - имя пользователя экранируется в фильтре (LDAP-инъекция).
func TestSearchModeEscapesFilter(t *testing.T) {
	dir := newDirectory()

	provider, err := New(searchConfig(), dir.dial)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := provider.CheckPassword(context.Background(), "*)(uid=*", "ivanov-pass"); ok {
		t.Fatal("инъекция в фильтр прошла")
	}

	if got := dir.filters[0]; !strings.Contains(got, `\2a\29\28uid=\2a`) {
		t.Fatalf("фильтр не экранирован: %q", got)
	}
}

// TestSearchModeServiceBindFails - неверный bind_password: ошибка провайдера (не отказ).
func TestSearchModeServiceBindFails(t *testing.T) {
	cfg := searchConfig()
	cfg.BindPassword = "wrong"

	provider, err := New(cfg, newDirectory().dial)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := provider.CheckPassword(context.Background(), "ivanov", "ivanov-pass"); ok || err == nil {
		t.Fatalf("CheckPassword = %v, %v; ожидали ошибку служебного bind", ok, err)
	}
}

// TestSimpleMode - без bind_dn DN собирается из uid и base.
func TestSimpleMode(t *testing.T) {
	dir := newDirectory()
	cfg := searchConfig()
	cfg.BindDN, cfg.BindPassword, cfg.Filter = "", "", ""

	provider, err := New(cfg, dir.dial)
	if err != nil {
		t.Fatal(err)
	}

	identity, ok, err := provider.CheckPassword(context.Background(), "ivanov", "ivanov-pass")
	if err != nil || !ok {
		t.Fatalf("CheckPassword = %v, %v", ok, err)
	}

	if dir.binds[0] != "uid=ivanov,ou=people,dc=example,dc=org" || identity.DisplayName != "Иванов Иван" {
		t.Fatalf("binds = %v, identity = %+v", dir.binds, identity)
	}

	if _, ok, err := provider.CheckPassword(context.Background(), "ivanov", "wrong"); ok || err != nil {
		t.Fatalf("неверный пароль = %v, %v", ok, err)
	}

	// Спецсимволы DN экранируются: "ivanov,ou=x" не превращается в другой DN.
	if _, ok, _ := provider.CheckPassword(context.Background(), "ivanov,ou=people", "ivanov-pass"); ok {
		t.Fatal("подмена DN прошла")
	}
}

// TestFailover - недоступный первый сервер, вход через второй; StartTLS по ldap://.
func TestFailover(t *testing.T) {
	dir := newDirectory()
	cfg := searchConfig()
	cfg.URI = config.StringList{"ldap://down.example.org", "ldap://ldap2.example.org"}
	cfg.StartTLS = true
	dir.down["ldap://down.example.org"] = true

	provider, err := New(cfg, dir.dial)
	if err != nil {
		t.Fatal(err)
	}

	if _, ok, err := provider.CheckPassword(context.Background(), "ivanov", "ivanov-pass"); !ok || err != nil {
		t.Fatalf("CheckPassword = %v, %v", ok, err)
	}

	if len(dir.dialed) != 2 || dir.startTLS != 1 {
		t.Fatalf("dialed = %v, startTLS = %d", dir.dialed, dir.startTLS)
	}

	dir.down["ldap://ldap2.example.org"] = true

	if _, _, err := provider.CheckPassword(context.Background(), "ivanov", "ivanov-pass"); err == nil {
		t.Fatal("все серверы недоступны - ожидали ошибку")
	}
}

// TestFromConfigSkipsDisabled - выключенный провайдер не собирается; bind_password_file читается.
func TestFromConfigSkipsDisabled(t *testing.T) {
	disabled := false
	off := searchConfig()
	off.Enabled = &disabled

	file := t.TempDir() + "/bind"
	if err := writeFile(file, "from-file\n"); err != nil {
		t.Fatal(err)
	}

	on := searchConfig()
	on.BindPassword = ""
	on.BindPasswordFile = file

	providers, err := FromConfig([]config.LDAPProvider{off, on})
	if err != nil {
		t.Fatal(err)
	}

	if len(providers) != 1 || providers[0].bindPassword != "from-file" {
		t.Fatalf("providers = %d", len(providers))
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
