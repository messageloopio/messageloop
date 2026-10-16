package proxyserver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPayloadTextHelpers(t *testing.T) {
	assert := assert.New(t)
	p := NewTextPayload("hello")
	assert.Equal("text/plain", p.ContentType)
	assert.Equal("hello", p.GetText())
	assert.Equal("hello", PayloadText(p))
	assert.Empty(PayloadText(nil))
	assert.Nil(PayloadBinary(p), "text payload has no binary variant")
}

func TestPayloadBinaryHelpers(t *testing.T) {
	assert := assert.New(t)
	p := NewBinaryPayload([]byte{1, 2, 3})
	assert.Equal("application/octet-stream", p.ContentType)
	assert.Equal([]byte{1, 2, 3}, PayloadBinary(p))
	assert.Nil(PayloadBinary(nil))
	assert.Empty(PayloadText(p), "binary payload has no text variant")
}

func TestPayloadJSONRoundTrip(t *testing.T) {
	assert := assert.New(t)

	p, err := NewJSONPayload(map[string]any{"user": "u1", "level": float64(7)})
	require.NoError(t, err)
	assert.Equal("application/json", p.ContentType)
	require.NotNil(t, p.GetJson())

	var into struct {
		User  string `json:"user"`
		Level int    `json:"level"`
	}
	require.NoError(t, PayloadJSON(p, &into))
	assert.Equal("u1", into.User)
	assert.Equal(7, into.Level)

	// Text payloads do not silently decode as JSON.
	err = PayloadJSON(NewTextPayload(`{"user":"u2"}`), &into)
	assert.Error(err)
	err = PayloadJSON(nil, &into)
	assert.Error(err)
}

func TestPayloadContentTypes(t *testing.T) {
	assert := assert.New(t)
	p, err := NewJSONPayload(map[string]any{})
	require.NoError(t, err)
	assert.Equal("application/json", p.ContentType)
}
