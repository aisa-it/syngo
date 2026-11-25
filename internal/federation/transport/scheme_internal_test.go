package transport

import (
	"net/http"
	"testing"
)

// TestClientSchemeAndInsecure — стенд: scheme http и отключение проверки TLS-сертификата.
func TestClientSchemeAndInsecure(t *testing.T) {
	t.Parallel()

	client := NewClient(Options{Scheme: "http", InsecureSkipVerify: true})
	if client.opts.Resolver.Scheme != "http" {
		t.Fatalf("resolver.Scheme = %q", client.opts.Resolver.Scheme)
	}

	tr, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", client.http.Transport)
	}

	if tr.TLSClientConfig == nil || !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("TLSClientConfig.InsecureSkipVerify не выставлен")
	}

	// Значение по умолчанию — https, проверка сертификата включена.
	secure := NewClient(Options{})
	if secure.opts.Resolver.Scheme != "https" {
		t.Fatalf("default resolver.Scheme = %q", secure.opts.Resolver.Scheme)
	}

	trSecure, ok := secure.http.Transport.(*http.Transport)
	if ok && trSecure.TLSClientConfig != nil && trSecure.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("по умолчанию проверка сертификата должна быть включена")
	}
}
