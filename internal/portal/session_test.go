// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/remotecommand"
)

func TestTerminalSessionBridgesInputOutputAndResize(t *testing.T) {
	var stateMu sync.Mutex
	var input string
	var size remotecommand.TerminalSize
	requestReceived := make(chan ExecRequest, 1)
	executor := executorFunc(func(_ context.Context, options remotecommand.StreamOptions) error {
		terminalSize := options.TerminalSizeQueue.Next()
		buffer := make([]byte, len("echo ready\n"))
		_, err := io.ReadFull(options.Stdin, buffer)
		if err != nil {
			return err
		}
		stateMu.Lock()
		input = string(buffer)
		size = *terminalSize
		stateMu.Unlock()
		_, err = options.Stdout.Write([]byte("ready\r\n"))
		return err
	})
	server := newTerminalTestServer(t, t.Context(), executorFactoryFunc(func(request ExecRequest) (remotecommand.Executor, error) {
		requestReceived <- request
		return executor, nil
	}), logr.Discard())
	conn := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "user-command")

	require.NoError(t, conn.WriteJSON(ClientControl{Type: "resize", Cols: 120, Rows: 40}))
	require.NoError(t, conn.WriteJSON(ClientControl{Type: "input", Data: "echo ready\n"}))
	messageType, output, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, messageType)
	assert.Equal(t, "ready\r\n", string(output))

	stateMu.Lock()
	assert.Equal(t, remotecommand.TerminalSize{Width: 120, Height: 40}, size)
	assert.Equal(t, "echo ready\n", input)
	stateMu.Unlock()
	request := <-requestReceived
	assert.Equal(t, []string{"/bin/sh"}, request.Command)
	assert.True(t, request.Stdin)
	assert.True(t, request.Stdout)
	assert.True(t, request.TTY)
	assert.Equal(t, "adopted-pod", request.Target.PodName)
}

func TestTerminalRejectsMissingCrossOriginAndMalformedOriginBeforeUpgrade(t *testing.T) {
	tests := []struct {
		name   string
		origin string
	}{
		{name: "missing"},
		{name: "cross origin", origin: "https://attacker.example"},
		{name: "malformed", origin: "://malformed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := newTerminalFixture()
			resolver, sandboxClient, claimClient, podClient := fixture.resolver(t, TerminalScope{Namespace: "team-a"}, time.Unix(500, 0))
			factoryCalled := false
			terminal := NewTerminalHandler(t.Context(), resolver, executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
				factoryCalled = true
				return nil, errors.New("must not be called")
			}), logr.Discard())
			server := newHTTPTestServer(t, ServerOptions{Terminal: terminal})

			_, response, err := dialTerminalResponse(t, server.URL, tt.origin, "team-a", "box-a", "workspace", "")
			require.Error(t, err)
			require.NotNil(t, response)
			defer response.Body.Close()
			assert.Equal(t, http.StatusForbidden, response.StatusCode)
			assert.Empty(t, sandboxClient.Actions())
			assert.Empty(t, claimClient.Actions())
			assert.Empty(t, podClient.Actions())
			assert.False(t, factoryCalled)
		})
	}
}

func TestTerminalRejectsOversizedAndUnknownControlMessages(t *testing.T) {
	executor := executorFunc(func(ctx context.Context, _ remotecommand.StreamOptions) error {
		<-ctx.Done()
		return ctx.Err()
	})
	server := newTerminalTestServer(t, t.Context(), executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return executor, nil
	}), logr.Discard())

	t.Run("oversized", func(t *testing.T) {
		conn := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat("x", 65*1024))))
		_, _, err := conn.ReadMessage()
		var closeError *websocket.CloseError
		require.ErrorAs(t, err, &closeError)
		assert.Equal(t, websocket.CloseMessageTooBig, closeError.Code)
	})

	t.Run("unknown", func(t *testing.T) {
		conn := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")
		require.NoError(t, conn.WriteJSON(ClientControl{Type: "launch", Data: "user-command"}))
		assertServerControl(t, conn, ServerControl{Type: "error", Message: "invalid terminal control message"})
	})
}

func TestTerminalUsesBinaryFramesForOutputAndJSONForExit(t *testing.T) {
	executor := executorFunc(func(_ context.Context, options remotecommand.StreamOptions) error {
		_, err := options.Stdout.Write([]byte{0, 1, 2, '\n'})
		return err
	})
	server := newTerminalTestServer(t, t.Context(), executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return executor, nil
	}), logr.Discard())
	conn := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")

	messageType, output, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, messageType)
	assert.Equal(t, []byte{0, 1, 2, '\n'}, output)
	assertServerControl(t, conn, ServerControl{Type: "exit"})
}

func TestTerminalStreamErrorSendsSafeControlMessage(t *testing.T) {
	var logs strings.Builder
	logged := make(chan struct{}, 1)
	logger := funcr.New(func(prefix, args string) {
		logs.WriteString(prefix)
		logs.WriteString(args)
		logged <- struct{}{}
	}, funcr.Options{})
	executor := executorFunc(func(context.Context, remotecommand.StreamOptions) error {
		return errors.New("remote stream leaked token=credential and workload output")
	})
	server := newTerminalTestServer(t, t.Context(), executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return executor, nil
	}), logger)
	conn := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")

	assertServerControl(t, conn, ServerControl{Type: "error", Message: "terminal session ended"})
	select {
	case <-logged:
	case <-time.After(time.Second):
		t.Fatal("terminal result was not logged")
	}
	assert.Contains(t, logs.String(), "exec-error")
	assert.Contains(t, logs.String(), "team-a")
	assert.Contains(t, logs.String(), "box-a")
	assert.NotContains(t, logs.String(), "credential")
	assert.NotContains(t, logs.String(), "workload output")
}

func TestTerminalDisconnectCancelsExec(t *testing.T) {
	started := make(chan struct{})
	cancelled := make(chan struct{})
	executor := executorFunc(func(ctx context.Context, _ remotecommand.StreamOptions) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	server := newTerminalTestServer(t, t.Context(), executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return executor, nil
	}), logr.Discard())
	conn := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")

	<-started
	require.NoError(t, conn.WriteJSON(ClientControl{Type: "input", Data: "blocked input"}))
	require.NoError(t, conn.Close())
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("exec context was not cancelled after WebSocket disconnect")
	}
}

func TestTerminalServerShutdownCancelsExec(t *testing.T) {
	serverContext, cancelServer := context.WithCancel(context.Background())
	t.Cleanup(cancelServer)
	cancelled := make(chan struct{})
	executor := executorFunc(func(ctx context.Context, _ remotecommand.StreamOptions) error {
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})
	server := newTerminalTestServer(t, serverContext, executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return executor, nil
	}), logr.Discard())
	_ = dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")

	cancelServer()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("exec context was not cancelled after server shutdown")
	}
}

func TestTerminalSessionsAreIndependent(t *testing.T) {
	factory := executorFactoryFunc(func(ExecRequest) (remotecommand.Executor, error) {
		return executorFunc(func(_ context.Context, options remotecommand.StreamOptions) error {
			buffer := make([]byte, 5)
			_, err := io.ReadFull(options.Stdin, buffer)
			if err != nil {
				return err
			}
			_, err = options.Stdout.Write(append([]byte("got:"), buffer...))
			return err
		}), nil
	})
	server := newTerminalTestServer(t, t.Context(), factory, logr.Discard())
	first := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")
	second := dialTerminal(t, server.URL, terminalOrigin(t, server.URL), "team-a", "box-a", "workspace", "")

	require.NoError(t, first.WriteJSON(ClientControl{Type: "input", Data: "first"}))
	require.NoError(t, second.WriteJSON(ClientControl{Type: "input", Data: "other"}))
	assertBinaryMessage(t, first, "got:first")
	assertBinaryMessage(t, second, "got:other")
}

func TestResizeQueueCoalescesWithoutBlocking(t *testing.T) {
	queue := newResizeQueue()
	updated := make(chan struct{})
	go func() {
		defer close(updated)
		for value := 1; value <= 1000; value++ {
			queue.Update(remotecommand.TerminalSize{Width: uint16(value), Height: uint16(value + 1)})
		}
	}()

	select {
	case <-updated:
	case <-time.After(time.Second):
		t.Fatal("resize updates blocked on a full queue")
	}
	assert.Equal(t, &remotecommand.TerminalSize{Width: 1000, Height: 1001}, queue.Next())
	queue.Close()
	assert.Nil(t, queue.Next())
}

type executorFactoryFunc func(ExecRequest) (remotecommand.Executor, error)

func (f executorFactoryFunc) NewExecutor(request ExecRequest) (remotecommand.Executor, error) {
	return f(request)
}

type executorFunc func(context.Context, remotecommand.StreamOptions) error

func (f executorFunc) Stream(options remotecommand.StreamOptions) error {
	return f(context.Background(), options)
}

func (f executorFunc) StreamWithContext(ctx context.Context, options remotecommand.StreamOptions) error {
	return f(ctx, options)
}

func newTerminalTestServer(t *testing.T, rootContext context.Context, factory ExecutorFactory, logger logr.Logger) *httptest.Server {
	t.Helper()
	fixture := newTerminalFixture()
	resolver, _, _, _ := fixture.resolver(t, TerminalScope{Namespace: "team-a"}, time.Unix(500, 0))
	terminal := NewTerminalHandler(rootContext, resolver, factory, logger)
	return newHTTPTestServer(t, ServerOptions{Terminal: terminal})
}

func dialTerminal(
	t *testing.T,
	serverURL string,
	origin string,
	namespace string,
	sandboxName string,
	container string,
	extraQuery string,
) *websocket.Conn {
	t.Helper()
	conn, response, err := dialTerminalResponse(t, serverURL, origin, namespace, sandboxName, container, extraQuery)
	if response != nil {
		response.Body.Close()
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func dialTerminalResponse(
	t *testing.T,
	serverURL string,
	origin string,
	namespace string,
	sandboxName string,
	container string,
	extraQuery string,
) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	query := url.Values{}
	query.Set("container", container)
	if extraQuery != "" {
		query.Set("command", extraQuery)
	}
	websocketURL := "ws" + strings.TrimPrefix(serverURL, "http") + fmt.Sprintf(
		"/api/v1/namespaces/%s/sandboxes/%s/terminal?%s",
		namespace,
		sandboxName,
		query.Encode(),
	)
	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.DefaultDialer.DialContext(t.Context(), websocketURL, header)
}

func terminalOrigin(t *testing.T, serverURL string) string {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	require.NoError(t, err)
	return "http://" + parsed.Host
}

func assertBinaryMessage(t *testing.T, conn *websocket.Conn, want string) {
	t.Helper()
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.BinaryMessage, messageType)
	assert.Equal(t, want, string(message))
}

func assertServerControl(t *testing.T, conn *websocket.Conn, want ServerControl) {
	t.Helper()
	messageType, message, err := conn.ReadMessage()
	require.NoError(t, err)
	assert.Equal(t, websocket.TextMessage, messageType)
	var got ServerControl
	require.NoError(t, json.Unmarshal(message, &got))
	assert.Equal(t, want, got)
}
