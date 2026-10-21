package vfs

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiskCacheStatsWithoutCache(t *testing.T) {
	require.Nil(t, (&VFS{}).DiskCacheStats())
}
