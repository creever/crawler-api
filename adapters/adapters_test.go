package adapters

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGet(t *testing.T) {
	a, err := Get("cipomarket")
	if err != nil {
		t.Fatalf("Get(cipomarket) error: %v", err)
	}
	if a.Name() != "cipomarket" {
		t.Errorf("Name() = %q, want cipomarket", a.Name())
	}
	if _, err := Get("nonexistent"); err == nil {
		t.Error("Get(nonexistent) should return an error")
	}
}

func TestCipomarketApply(t *testing.T) {
	var gotBody map[string]string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	a, _ := Get("cipomarket")
	err := a.Apply(context.Background(), srv.URL, "secret-key", Change{
		Kind:  "title",
		Page:  "https://cipomarket.hu/noi-cipok/",
		Value: "Női cipők | Cipomarket",
	})
	if err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want Bearer secret-key", gotAuth)
	}
	if gotBody["kind"] != "title" || gotBody["page"] != "https://cipomarket.hu/noi-cipok/" || gotBody["value"] != "Női cipők | Cipomarket" {
		t.Errorf("unexpected body: %v", gotBody)
	}
}

func TestCipomarketApplyServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	a, _ := Get("cipomarket")
	err := a.Apply(context.Background(), srv.URL, "", Change{Kind: "title", Page: "/x", Value: "y"})
	if err == nil {
		t.Error("expected error on HTTP 500")
	}
}

func TestCipomarketApplyNoEndpoint(t *testing.T) {
	a, _ := Get("cipomarket")
	if err := a.Apply(context.Background(), "", "", Change{}); err == nil {
		t.Error("expected error when endpoint is empty")
	}
}
