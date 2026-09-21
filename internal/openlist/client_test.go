package openlist

import (
	"bytes"
	"context"
	"testing"

	"github.com/sagerenn/openlist/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClientPingAndAuth(t *testing.T) {
	stub := testutil.NewStubOpenList("tok-123")
	defer stub.Close()

	c := New(stub.URL(), "tok-123")
	require.NoError(t, c.Ping(context.Background()))

	// Wrong token => error.
	c2 := New(stub.URL(), "wrong")
	require.Error(t, c2.Ping(context.Background()))
}

func TestClientRemoveFile(t *testing.T) {
	stub := testutil.NewStubOpenList("tok-123")
	defer stub.Close()

	c := New(stub.URL(), "tok-123")
	require.NoError(t, c.RemoveFile(context.Background(), "/photos/a.jpg"))

	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1)
	assert.Equal(t, "/photos", removes[0].Dir)
	assert.Equal(t, []string{"a.jpg"}, removes[0].Names)
}

func TestClientRemoveFileRootLevel(t *testing.T) {
	stub := testutil.NewStubOpenList("tok-123")
	defer stub.Close()

	c := New(stub.URL(), "tok-123")
	require.NoError(t, c.RemoveFile(context.Background(), "rootfile.txt"))

	removes := stub.SnapshotRemoves()
	require.Len(t, removes, 1)
	assert.Equal(t, "/", removes[0].Dir)
	assert.Equal(t, []string{"rootfile.txt"}, removes[0].Names)
}

func TestClientUploadFile(t *testing.T) {
	stub := testutil.NewStubOpenList("tok-123")
	defer stub.Close()

	c := New(stub.URL(), "tok-123")
	body := bytes.NewReader([]byte("file-content"))
	require.NoError(t, c.UploadFile(context.Background(), "/up/test.txt", body, int64(len("file-content"))))

	uploads := stub.SnapshotUploads()
	require.Len(t, uploads, 1)
	assert.Equal(t, "/up/test.txt", uploads[0].Path)
	assert.Equal(t, "file-content", uploads[0].Body)
	assert.Equal(t, "tok-123", uploads[0].Token)
}

func TestClientRemoveEmptyPathErrors(t *testing.T) {
	stub := testutil.NewStubOpenList("tok-123")
	defer stub.Close()
	c := New(stub.URL(), "tok-123")
	require.Error(t, c.RemoveFile(context.Background(), "/"))
}
