package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	SettingKeyDistillDriveRefreshToken = "distill_drive_refresh_token"
	SettingKeyDistillDriveClientSecret = "distill_drive_client_secret"
	distillGoogleTokenURL              = "https://oauth2.googleapis.com/token"
)

// DistillCredentials loads encrypted Drive OAuth secrets and refreshes access tokens.
type DistillCredentials struct {
	repo      SettingRepository
	encryptor SecretEncryptor
	clientID  string
	client    *http.Client

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

func NewDistillCredentials(repo SettingRepository, encryptor SecretEncryptor, clientID string) *DistillCredentials {
	return &DistillCredentials{
		repo:      repo,
		encryptor: encryptor,
		clientID:  strings.TrimSpace(clientID),
		client:    &http.Client{Timeout: 5 * time.Minute},
	}
}

func (d *DistillCredentials) Save(ctx context.Context, refreshToken, clientSecret string) error {
	if d == nil || d.repo == nil || d.encryptor == nil {
		return errors.New("distill credentials: not configured")
	}
	encRefresh, err := d.encryptor.Encrypt(refreshToken)
	if err != nil {
		return err
	}
	encSecret, err := d.encryptor.Encrypt(clientSecret)
	if err != nil {
		return err
	}
	if err := d.repo.Set(ctx, SettingKeyDistillDriveRefreshToken, encRefresh); err != nil {
		return err
	}
	return d.repo.Set(ctx, SettingKeyDistillDriveClientSecret, encSecret)
}

func (d *DistillCredentials) Load(ctx context.Context) (refreshToken, clientSecret string, err error) {
	if d == nil || d.repo == nil || d.encryptor == nil {
		return "", "", errors.New("distill credentials: not configured")
	}
	rawRefresh, err := d.repo.GetValue(ctx, SettingKeyDistillDriveRefreshToken)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return "", "", err
	}
	rawSecret, err := d.repo.GetValue(ctx, SettingKeyDistillDriveClientSecret)
	if err != nil && !errors.Is(err, ErrSettingNotFound) {
		return "", "", err
	}
	if rawRefresh == "" && rawSecret == "" {
		return "", "", nil
	}
	if rawRefresh != "" {
		refreshToken, err = d.encryptor.Decrypt(rawRefresh)
		if err != nil {
			return "", "", err
		}
	}
	if rawSecret != "" {
		clientSecret, err = d.encryptor.Decrypt(rawSecret)
		if err != nil {
			return "", "", err
		}
	}
	return refreshToken, clientSecret, nil
}

// AccessToken returns a cached Google access token, refreshing when needed.
func (d *DistillCredentials) AccessToken(ctx context.Context) (string, error) {
	if d == nil {
		return "", errors.New("distill credentials: nil")
	}
	d.mu.Lock()
	if d.accessToken != "" && time.Now().Before(d.expiresAt) {
		tok := d.accessToken
		d.mu.Unlock()
		return tok, nil
	}
	d.mu.Unlock()

	refresh, secret, err := d.Load(ctx)
	if err != nil {
		return "", err
	}
	if refresh == "" || secret == "" || d.clientID == "" {
		return "", errors.New("distill credentials: incomplete oauth configuration")
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refresh)
	form.Set("client_id", d.clientID)
	form.Set("client_secret", secret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, distillGoogleTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("distill credentials: token refresh failed: " + strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("distill credentials: empty access_token")
	}
	expiresIn := out.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 3600
	}
	expiry := time.Now().Add(time.Duration(expiresIn) * time.Second).Add(-60 * time.Second)

	d.mu.Lock()
	d.accessToken = out.AccessToken
	d.expiresAt = expiry
	d.mu.Unlock()
	return out.AccessToken, nil
}
