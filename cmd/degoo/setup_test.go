//go:build linux

package degoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rclone/rclone/backend/degoo/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAncestorFolders(t *testing.T) {
	items := map[string]api.Item{
		"vault":  {ID: "vault", Name: "Private vault", ParentID: "device", Category: 2, URL: "secret-signed-url"},
		"device": {ID: "device", Name: "Device", ParentID: "0", Category: 1},
	}
	get := func(_ context.Context, id string) (api.Item, error) { return items[id], nil }
	entries, err := ancestorFolders(context.Background(), get, "vault")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "Device", entries[0].Name)
	assert.Equal(t, "Private vault", entries[1].Name)
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "secret-signed-url")
	items["device"] = api.Item{ID: "device", ParentID: "vault"}
	_, err = ancestorFolders(context.Background(), get, "vault")
	require.ErrorContains(t, err, "cyclic")
	_, err = ancestorFolders(context.Background(), get, "missing")
	require.ErrorContains(t, err, "mismatched")
	_, err = ancestorFolders(context.Background(), func(context.Context, string) (api.Item, error) { return api.Item{}, errors.New("unavailable") }, "vault")
	require.EqualError(t, err, "unavailable")
}

func TestWriteFolders(t *testing.T) {
	var out bytes.Buffer
	err := writeFolders(&out, []api.Item{
		{ID: "1", Name: "Device", Category: 1, URL: "secret-url"},
		{ID: "2", Name: "Vault", Category: 2},
		{ID: "3", Name: "Native", Category: 6},
		{ID: "4", Name: "Personal file", Category: 0, URL: "secret-url"},
	})
	require.NoError(t, err)
	var entries []folderLocation
	require.NoError(t, json.Unmarshal(out.Bytes(), &entries))
	require.Len(t, entries, 3)
	assert.NotContains(t, out.String(), "secret-url")
	assert.NotContains(t, out.String(), "Personal file")
}

func TestValidSetupName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", "a\\b", "a\x00b", " Vault", ".rclone-degoo-upload-danger"} {
		assert.False(t, validSetupName(name), name)
	}
	assert.True(t, validSetupName("Private vault"))
}
