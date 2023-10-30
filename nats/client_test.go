package nats

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHopsNats(t *testing.T) {
	ctx := context.Background()
	hopsNats, cleanup := setupHopsNats(ctx, t)
	defer cleanup()

	if assert.NotNil(t, hopsNats) {
		defer hopsNats.Close()
	}

	if assert.NotNil(t, hopsNats.NatsConn) {
		assert.True(t, hopsNats.NatsConn.IsConnected(), "HopsNats should be connected to NATS server")
	}

	assert.NotNil(t, hopsNats.JetStream, "HopsNats should initialise JetStream")
	assert.NotNil(t, hopsNats.Consumer, "HopsNats should initialise the Consumer")
}

func TestHopsNatsConsume(t *testing.T) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	hopsNats, cleanup := setupHopsNats(ctx, t)
	defer cleanup()

	type testMsg struct {
		subject string
		data    []byte
	}

	receivedChan := make(chan testMsg)

	go func() {
		hopsNats.Consume(ctx, func(m jetstream.Msg) {
			m.DoubleAck(ctx) // Ack before logging to avoid race condition in tests
			receivedChan <- testMsg{
				subject: m.Subject(),
				data:    m.Data(),
			}
		})
	}()

	_, err := hopsNats.Publish(ctx, []byte("Hello world"), ChannelNotify, "SEQ_ID", "MSG_ID")
	if assert.NoError(t, err, "Message should be published without errror") {
		receivedMsg := <-receivedChan
		assert.Contains(t, receivedMsg.subject, "SEQ_ID.MSG_ID")
		assert.Equal(t, []byte("Hello world"), receivedMsg.data)
	}
}

// setupHopsNats is a test helper to create an instance of HopsNats with a local NATS server
func setupHopsNats(ctx context.Context, t *testing.T) (*Client, func()) {
	localNats := setupLocalNatsServer(t)

	authUrl, err := localNats.AuthUrl("")
	require.NoError(t, err, "Test setup: Should have valid auth URL for NATS")

	user, err := localNats.User("")
	require.NoError(t, err, "Test setup: Should have valid NATS user")

	hopsNats, err := NewClient(ctx, authUrl, user.Account.Name)
	require.NoError(t, err, "Test setup: HopsNats should initialise without error")

	cleanup := func() {
		hopsNats.Close()
		localNats.Close()
	}

	return hopsNats, cleanup
}
