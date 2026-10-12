package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type fakeDistillCredentialsSaver struct {
	calls        int
	refreshToken string
	clientSecret string
}

func (f *fakeDistillCredentialsSaver) Save(_ context.Context, refreshToken, clientSecret string) error {
	f.calls++
	f.refreshToken = refreshToken
	f.clientSecret = clientSecret
	return nil
}

func TestUpdateDistillDriveCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("valid credentials", func(t *testing.T) {
		fake := &fakeDistillCredentialsSaver{}
		h := &SettingHandler{}
		h.SetDistillCredentials(fake)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := `{"refresh_token":"rt","client_secret":"cs"}`
		c.Request = httptest.NewRequest(http.MethodPut, "/distill-drive-credentials", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")

		h.UpdateDistillDriveCredentials(c)

		require.Equal(t, http.StatusNoContent, w.Code)
		require.Equal(t, 1, fake.calls)
		require.Equal(t, "rt", fake.refreshToken)
		require.Equal(t, "cs", fake.clientSecret)
		require.NotContains(t, w.Body.String(), "rt")
		require.NotContains(t, w.Body.String(), "cs")
	})

	t.Run("empty refresh_token", func(t *testing.T) {
		fake := &fakeDistillCredentialsSaver{}
		h := &SettingHandler{}
		h.SetDistillCredentials(fake)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := `{"refresh_token":"","client_secret":"cs"}`
		c.Request = httptest.NewRequest(http.MethodPut, "/distill-drive-credentials", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")

		h.UpdateDistillDriveCredentials(c)

		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, 0, fake.calls)
		require.NotContains(t, strings.ToLower(w.Body.String()), "rt")
	})

	t.Run("empty client_secret", func(t *testing.T) {
		fake := &fakeDistillCredentialsSaver{}
		h := &SettingHandler{}
		h.SetDistillCredentials(fake)

		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		body := `{"refresh_token":"rt","client_secret":""}`
		c.Request = httptest.NewRequest(http.MethodPut, "/distill-drive-credentials", bytes.NewBufferString(body))
		c.Request.Header.Set("Content-Type", "application/json")

		h.UpdateDistillDriveCredentials(c)

		require.Equal(t, http.StatusBadRequest, w.Code)
		require.Equal(t, 0, fake.calls)
	})
}
