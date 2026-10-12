package distill

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const testFolderID = "distill-folder-id"

func testToken(ctx context.Context) (string, error) {
	return "test-access-token", nil
}

func newDriveTestServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(handler))
}

func newHTTPDriveForTest(server *httptest.Server) *HTTPDrive {
	return &HTTPDrive{
		FolderID:   testFolderID,
		Token:      testToken,
		UploadBase: server.URL,
		DriveBase:  server.URL,
	}
}

func TestHTTPDriveUpload(t *testing.T) {
	const wantID = "drive-file-abc"
	var putBody []byte

	srv := newDriveTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/drive/v3/files"):
			if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
				t.Errorf("Authorization = %q, want Bearer test-access-token", got)
			}
			if ct := r.Header.Get("X-Upload-Content-Type"); ct != "application/x-ndjson" {
				t.Errorf("X-Upload-Content-Type = %q", ct)
			}
			if cl := r.Header.Get("X-Upload-Content-Length"); cl != "5" {
				t.Errorf("X-Upload-Content-Length = %q, want 5", cl)
			}
			var meta struct {
				Name    string   `json:"name"`
				Parents []string `json:"parents"`
			}
			if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
				t.Fatalf("decode meta: %v", err)
			}
			if meta.Name != "part.jsonl" || len(meta.Parents) != 1 || meta.Parents[0] != testFolderID {
				t.Fatalf("meta = %+v", meta)
			}
			w.Header().Set("Location", uploadSessionURL(r))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read put body: %v", err)
			}
			putBody = data
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + wantID + `"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	})
	defer srv.Close()

	d := newHTTPDriveForTest(srv)
	content := []byte("hello")
	id, err := d.Upload(context.Background(), "part.jsonl", strings.NewReader(string(content)), int64(len(content)))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if id != wantID {
		t.Fatalf("id = %q, want %q", id, wantID)
	}
	if string(putBody) != "hello" {
		t.Fatalf("put body = %q", putBody)
	}
}

func uploadSessionURL(r *http.Request) string {
	scheme := r.URL.Scheme
	if scheme == "" {
		scheme = "http"
	}
	return scheme + "://" + r.Host + "/upload/session"
}

func TestReconcileUploadsAndDeletesAfterLedger(t *testing.T) {
	dir := t.TempDir()
	closedName := "2026-10-11-001.jsonl"
	openName := "2026-10-12-001.jsonl"
	closedPath := filepath.Join(dir, closedName)
	if err := os.WriteFile(closedPath, []byte("line\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, openName), []byte("open\n"), 0600); err != nil {
		t.Fatal(err)
	}

	const fileID = "uploaded-id-1"
	var uploadCount atomic.Int32
	ledgerPath := filepath.Join(dir, "uploaded.json")

	srv := newDriveTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/drive/v3/files"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/drive/v3/files"):
			w.Header().Set("Location", uploadSessionURL(r))
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			if _, err := os.Stat(ledgerPath); err == nil {
				t.Error("ledger existed before upload finished")
			}
			if _, err := os.Stat(closedPath); err != nil {
				t.Error("jsonl missing during upload")
			}
			uploadCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"` + fileID + `"}`))
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	d := newHTTPDriveForTest(srv)
	if err := Reconcile(context.Background(), dir, openName, d); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if uploadCount.Load() != 1 {
		t.Fatalf("uploads = %d, want 1", uploadCount.Load())
	}

	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	var ledger map[string]string
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatalf("unmarshal ledger: %v", err)
	}
	if ledger[closedName] != fileID {
		t.Fatalf("ledger[%q] = %q, want %q", closedName, ledger[closedName], fileID)
	}
	if _, err := os.Stat(closedPath); !os.IsNotExist(err) {
		t.Fatalf("closed jsonl still exists: %v", err)
	}
}

func TestReconcileRestartWithLedgerSkipsUpload(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-11-001.jsonl"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ledger := map[string]string{name: "existing-id"}
	b, _ := json.Marshal(ledger)
	if err := os.WriteFile(filepath.Join(dir, "uploaded.json"), b, 0600); err != nil {
		t.Fatal(err)
	}

	var uploadCount atomic.Int32
	srv := newDriveTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		uploadCount.Add(1)
		t.Fatal("unexpected upload")
	})
	defer srv.Close()

	d := newHTTPDriveForTest(srv)
	if err := Reconcile(context.Background(), dir, "", d); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if uploadCount.Load() != 0 {
		t.Fatalf("uploads = %d", uploadCount.Load())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("jsonl should be removed: %v", err)
	}
}

func TestReconcileFindByNameBackfillsLedger(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-11-001.jsonl"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}

	const remoteID = "remote-file-id"
	var uploadBytes atomic.Int32

	srv := newDriveTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/drive/v3/files"):
			q := r.URL.Query().Get("q")
			wantQ := "name = '" + name + "' and '" + testFolderID + "' in parents and trashed = false"
			if q != wantQ {
				t.Errorf("q = %q, want %q", q, wantQ)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[{"id":"` + remoteID + `","name":"` + name + `"}]}`))
		case r.Method == http.MethodPost, r.Method == http.MethodPut:
			uploadBytes.Add(int32(len(mustReadAll(r.Body))))
			t.Fatal("unexpected upload")
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	d := newHTTPDriveForTest(srv)
	if err := Reconcile(context.Background(), dir, "", d); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if uploadBytes.Load() != 0 {
		t.Fatalf("upload body bytes = %d", uploadBytes.Load())
	}
	data, err := os.ReadFile(filepath.Join(dir, "uploaded.json"))
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	var ledger map[string]string
	if err := json.Unmarshal(data, &ledger); err != nil {
		t.Fatal(err)
	}
	if ledger[name] != remoteID {
		t.Fatalf("ledger id = %q", ledger[name])
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("jsonl should be removed")
	}
}

func TestReconcileUploadErrorKeepsJSONL(t *testing.T) {
	dir := t.TempDir()
	name := "2026-10-11-001.jsonl"
	path := filepath.Join(dir, name)
	content := []byte("data\n")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}

	srv := newDriveTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/drive/v3/files"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"files":[]}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/drive/v3/files"):
			w.Header().Set("Location", r.URL.Scheme+"://"+r.Host+"/upload/session")
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	})
	defer srv.Close()

	d := newHTTPDriveForTest(srv)
	err := Reconcile(context.Background(), dir, "", d)
	if err == nil {
		t.Fatal("expected error")
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("jsonl should remain: %v", statErr)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil || string(got) != string(content) {
		t.Fatalf("jsonl content changed")
	}
	if _, err := os.Stat(filepath.Join(dir, "uploaded.json")); err == nil {
		t.Fatal("ledger should not exist after failed upload")
	}
}

func mustReadAll(r io.Reader) []byte {
	b, err := io.ReadAll(r)
	if err != nil {
		panic(err)
	}
	return b
}
