package distill

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type DriveAPI interface {
	Upload(ctx context.Context, name string, r io.ReadSeeker, size int64) (fileID string, err error)
	FindByName(ctx context.Context, name string) (fileID string, ok bool, err error)
}

type HTTPDrive struct {
	FolderID string
	Token    func(ctx context.Context) (string, error)
	Client   *http.Client
	UploadBase string
	DriveBase  string
}

func (d *HTTPDrive) client() *http.Client {
	if d.Client != nil {
		return d.Client
	}
	return http.DefaultClient
}

func (d *HTTPDrive) uploadBase() string {
	if d.UploadBase != "" {
		return strings.TrimRight(d.UploadBase, "/")
	}
	return "https://www.googleapis.com"
}

func (d *HTTPDrive) driveBase() string {
	if d.DriveBase != "" {
		return strings.TrimRight(d.DriveBase, "/")
	}
	return "https://www.googleapis.com"
}

func (d *HTTPDrive) authHeader(ctx context.Context) (string, error) {
	if d.Token == nil {
		return "", fmt.Errorf("distill drive: missing token source")
	}
	tok, err := d.Token(ctx)
	if err != nil {
		return "", err
	}
	return "Bearer " + tok, nil
}

func (d *HTTPDrive) Upload(ctx context.Context, name string, r io.ReadSeeker, size int64) (string, error) {
	auth, err := d.authHeader(ctx)
	if err != nil {
		return "", err
	}

	startURL := d.uploadBase() + "/upload/drive/v3/files?uploadType=resumable&fields=id"
	meta := struct {
		Name    string   `json:"name"`
		Parents []string `json:"parents"`
	}{Name: name, Parents: []string{d.FolderID}}
	metaBody, err := json.Marshal(meta)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, startURL, bytes.NewReader(metaBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Type", "application/x-ndjson")
	req.Header.Set("X-Upload-Content-Length", fmt.Sprintf("%d", size))

	resp, err := d.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("distill drive: resumable start: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	location := resp.Header.Get("Location")
	if location == "" {
		return "", fmt.Errorf("distill drive: missing Location header")
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, location, r)
	if err != nil {
		return "", err
	}
	putReq.ContentLength = size
	putReq.Header.Set("Authorization", auth)
	putReq.Header.Set("Content-Type", "application/x-ndjson")

	putResp, err := d.client().Do(putReq)
	if err != nil {
		return "", err
	}
	defer putResp.Body.Close()
	if putResp.StatusCode < 200 || putResp.StatusCode >= 300 {
		body, _ := io.ReadAll(putResp.Body)
		return "", fmt.Errorf("distill drive: upload: %s: %s", putResp.Status, strings.TrimSpace(string(body)))
	}

	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(putResp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("distill drive: empty file id")
	}
	return out.ID, nil
}

func (d *HTTPDrive) FindByName(ctx context.Context, name string) (string, bool, error) {
	auth, err := d.authHeader(ctx)
	if err != nil {
		return "", false, err
	}

	q := fmt.Sprintf("name = '%s' and '%s' in parents and trashed = false", name, d.FolderID)
	u, err := url.Parse(d.driveBase() + "/drive/v3/files")
	if err != nil {
		return "", false, err
	}
	params := url.Values{}
	params.Set("q", q)
	params.Set("fields", "files(id,name)")
	params.Set("pageSize", "1")
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Authorization", auth)

	resp, err := d.client().Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return "", false, fmt.Errorf("distill drive: find: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var out struct {
		Files []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, err
	}
	if len(out.Files) == 0 {
		return "", false, nil
	}
	return out.Files[0].ID, true, nil
}

func ledgerPath(dir string) string {
	return filepath.Join(dir, "uploaded.json")
}

func loadLedger(dir string) (map[string]string, error) {
	path := ledgerPath(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]string{}
	}
	return m, nil
}

func saveLedger(dir string, ledger map[string]string) error {
	data, err := json.Marshal(ledger)
	if err != nil {
		return err
	}
	path := ledgerPath(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func listClosedJSONL(dir, openName string) ([]string, error) {
	spoolDir := filepath.Join(dir, "spool")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		if name == openName {
			continue
		}
		path := filepath.Join(dir, name)
		if inSpool, err := isUnderDir(path, spoolDir); err != nil {
			return nil, err
		} else if inSpool {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func isUnderDir(path, dir string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(absDir, absPath)
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)), nil
}

func Reconcile(ctx context.Context, dir, openName string, api DriveAPI) error {
	names, err := listClosedJSONL(dir, openName)
	if err != nil {
		return err
	}
	ledger, err := loadLedger(dir)
	if err != nil {
		return err
	}

	for _, name := range names {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}

		if id, ok := ledger[name]; ok && id != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}

		if id, ok, err := api.FindByName(ctx, name); err != nil {
			return err
		} else if ok {
			ledger[name] = id
			if err := saveLedger(dir, ledger); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		id, err := api.Upload(ctx, name, f, st.Size())
		f.Close()
		if err != nil {
			return err
		}
		ledger[name] = id
		if err := saveLedger(dir, ledger); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
