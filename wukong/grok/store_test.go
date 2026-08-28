package grok

import (
	"path/filepath"
	"testing"
)

func TestAccountStoreAddClearPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grok.json")
	store := NewAccountStore(path, Config{})
	added, err := store.ImportRaw([]byte("sso=one\nsso=two\n"))
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 || store.Count() != 2 {
		t.Fatalf("added=%d count=%d", added, store.Count())
	}
	again, err := store.ImportRaw([]byte(`{"sso":"one","name":"renamed"}`))
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("dup should not count as added: %d", again)
	}
	pubs := store.PublicAccounts()
	if len(pubs) != 2 || pubs[0].Name != "renamed" || !pubs[0].HasSSO {
		t.Fatalf("%#v", pubs)
	}
	loaded, err := LoadCredentialsFile(path)
	if err != nil || len(loaded) != 2 {
		t.Fatalf("persist: %v %#v", err, loaded)
	}

	changed := 0
	store.SetOnChange(func([]Credential) { changed++ })
	if err = store.Clear(); err != nil {
		t.Fatal(err)
	}
	if store.Count() != 0 || changed != 1 {
		t.Fatalf("clear count=%d changed=%d", store.Count(), changed)
	}
	loadedAfter, err := LoadCredentialsFileOptional(path)
	if err != nil || len(loadedAfter) != 0 {
		t.Fatalf("cleared file: %v %#v", err, loadedAfter)
	}
}
