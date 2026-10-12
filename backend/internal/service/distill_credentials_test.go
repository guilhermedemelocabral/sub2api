package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

type distillCredSettingRepo struct {
	vals map[string]string
}

func (r *distillCredSettingRepo) Get(ctx context.Context, key string) (*Setting, error) {
	v, ok := r.vals[key]
	if !ok {
		return nil, ErrSettingNotFound
	}
	return &Setting{Key: key, Value: v}, nil
}

func (r *distillCredSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	v, ok := r.vals[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return v, nil
}

func (r *distillCredSettingRepo) Set(ctx context.Context, key, value string) error {
	if r.vals == nil {
		r.vals = map[string]string{}
	}
	r.vals[key] = value
	return nil
}

func (r *distillCredSettingRepo) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, ok := r.vals[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (r *distillCredSettingRepo) SetMultiple(ctx context.Context, settings map[string]string) error {
	for k, v := range settings {
		if err := r.Set(ctx, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (r *distillCredSettingRepo) GetAll(ctx context.Context) (map[string]string, error) {
	out := make(map[string]string, len(r.vals))
	for k, v := range r.vals {
		out[k] = v
	}
	return out, nil
}

func (r *distillCredSettingRepo) Delete(ctx context.Context, key string) error {
	delete(r.vals, key)
	return nil
}

type prefixEncryptor struct{}

func (prefixEncryptor) Encrypt(plaintext string) (string, error) {
	return "enc:" + plaintext, nil
}

func (prefixEncryptor) Decrypt(ciphertext string) (string, error) {
	const p = "enc:"
	if len(ciphertext) >= len(p) && ciphertext[:len(p)] == p {
		return ciphertext[len(p):], nil
	}
	return ciphertext, nil
}

func TestDistillCredentialsSaveLoad(t *testing.T) {
	repo := &distillCredSettingRepo{vals: map[string]string{}}
	creds := NewDistillCredentials(repo, prefixEncryptor{}, "client-id")

	require.NoError(t, creds.Save(context.Background(), "refresh-plain", "secret-plain"))
	require.Equal(t, "enc:refresh-plain", repo.vals[SettingKeyDistillDriveRefreshToken])
	require.Equal(t, "enc:secret-plain", repo.vals[SettingKeyDistillDriveClientSecret])

	refresh, secret, err := creds.Load(context.Background())
	require.NoError(t, err)
	require.Equal(t, "refresh-plain", refresh)
	require.Equal(t, "secret-plain", secret)
}

func TestDistillCredentialsLoadEmpty(t *testing.T) {
	repo := &distillCredSettingRepo{vals: map[string]string{}}
	creds := NewDistillCredentials(repo, prefixEncryptor{}, "client-id")

	refresh, secret, err := creds.Load(context.Background())
	require.NoError(t, err)
	require.Empty(t, refresh)
	require.Empty(t, secret)
}
