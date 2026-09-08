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
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/gorilla/websocket"
	"k8s.io/client-go/tools/remotecommand"
)

const (
	terminalControlReadLimit = 64 * 1024
	terminalInputQueueSize   = 16
	terminalPongTimeout      = 60 * time.Second
	terminalPingInterval     = 30 * time.Second
	terminalWriteTimeout     = 10 * time.Second

	invalidTerminalOriginMessage  = "WebSocket Origin is not allowed"
	invalidTerminalControlMessage = "invalid terminal control message"
	terminalSessionEndedMessage   = "terminal session ended"
)

// TerminalHandler validates and bridges browser terminal WebSockets to Kubernetes exec.
type TerminalHandler struct {
	rootContext context.Context
	resolver    *TerminalResolver
	factory     ExecutorFactory
	log         logr.Logger
	upgrader    websocket.Upgrader
}

// NewTerminalHandler creates the same-origin terminal WebSocket endpoint.
func NewTerminalHandler(
	rootContext context.Context,
	resolver *TerminalResolver,
	factory ExecutorFactory,
	log logr.Logger,
) http.Handler {
	if rootContext == nil {
		rootContext = context.Background()
	}
	return &TerminalHandler{
		rootContext: rootContext,
		resolver:    resolver,
		factory:     factory,
		log:         log,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4 * 1024,
			WriteBufferSize: 4 * 1024,
			CheckOrigin:     sameOrigin,
		},
	}
}

// ServeHTTP resolves an authorized target before upgrading the connection.
func (h *TerminalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeAPIError(w, http.StatusForbidden, invalidTerminalOriginMessage)
		return
	}
	if h.resolver == nil || h.factory == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "terminal is temporarily unavailable")
		return
	}

	target, err := h.resolver.Resolve(
		r.Context(),
		r.PathValue("namespace"),
		r.PathValue("sandbox"),
		r.URL.Query().Get("container"),
	)
	if err != nil {
		var terminalErr *TerminalError
		if errors.As(err, &terminalErr) {
			writeAPIError(w, terminalErr.Status, terminalErr.Message)
		} else {
			writeAPIError(w, http.StatusServiceUnavailable, "terminal is temporarily unavailable")
		}
		return
	}

	executor, err := h.factory.NewExecutor(ExecRequest{
		Target:  target,
		Command: []string{fixedTerminalShell},
		Stdin:   true,
		Stdout:  true,
		TTY:     true,
	})
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "terminal is temporarily unavailable")
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	runTerminalSession(h.rootContext, conn, executor, target, h.log)
}

func sameOrigin(r *http.Request) bool {
	originValue := r.Header.Get("Origin")
	if originValue == "" {
		return false
	}
	origin, err := url.Parse(originValue)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.Host != r.Host {
		return false
	}
	return origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == "" &&
		loopbackHostname(origin.Hostname())
}

func loopbackHostname(hostname string) bool {
	if hostname == "" {
		return false
	}
	if strings.EqualFold(hostname, "localhost") {
		return true
	}
	ip := net.ParseIP(hostname)
	return ip != nil && ip.IsLoopback()
}

type terminalClientResult int

const (
	terminalClientDisconnected terminalClientResult = iota
	terminalClientInvalidControl
)

func runTerminalSession(
	rootContext context.Context,
	conn webSocketConnection,
	executor remotecommand.Executor,
	target TerminalTarget,
	log logr.Logger,
) {
	sessionContext, cancel := context.WithCancel(rootContext)
	stdinReader, stdinWriter := io.Pipe()
	input := make(chan []byte, terminalInputQueueSize)
	resizes := newResizeQueue()
	writer := &serializedWebSocketWriter{conn: conn, now: time.Now}
	var cleanupOnce sync.Once
	cleanupSession := func() {
		cleanupOnce.Do(func() {
			cancel()
			_ = stdinWriter.Close()
			_ = stdinReader.Close()
			resizes.Close()
		})
	}

	conn.SetReadLimit(terminalControlReadLimit)
	_ = conn.SetReadDeadline(time.Now().Add(terminalPongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(terminalPongTimeout))
	})

	execResult := make(chan error, 1)
	go func() {
		execResult <- executor.StreamWithContext(sessionContext, remotecommand.StreamOptions{
			Stdin:             stdinReader,
			Stdout:            &websocketOutput{writer: writer},
			Stderr:            nil,
			Tty:               true,
			TerminalSizeQueue: resizes,
		})
	}()
	clientResult := make(chan terminalClientResult, 2)
	go pumpTerminalInput(sessionContext, stdinWriter, input, clientResult)
	go readTerminalControls(conn, input, resizes, clientResult)

	pingTicker := time.NewTicker(terminalPingInterval)
	defer pingTicker.Stop()
	defer func() {
		cleanupSession()
		_ = conn.Close()
	}()

	for {
		select {
		case err := <-execResult:
			result := "clean-exit"
			cleanupSession()
			if err == nil {
				_ = writer.WriteJSON(ServerControl{Type: "exit"})
			} else if errors.Is(err, context.Canceled) {
				result = "cancelled"
			} else {
				result = "exec-error"
				_ = writer.WriteJSON(ServerControl{Type: "error", Message: terminalSessionEndedMessage})
			}
			logTerminalResult(log, target, result)
			return
		case result := <-clientResult:
			cleanupSession()
			if result == terminalClientInvalidControl {
				_ = writer.WriteJSON(ServerControl{Type: "error", Message: invalidTerminalControlMessage})
			}
			_ = conn.Close()
			<-execResult
			logTerminalResult(log, target, "disconnect")
			return
		case <-rootContext.Done():
			cleanupSession()
			_ = conn.Close()
			<-execResult
			logTerminalResult(log, target, "cancelled")
			return
		case <-pingTicker.C:
			if err := writer.WritePing(); err != nil {
				cleanupSession()
				_ = conn.Close()
				<-execResult
				logTerminalResult(log, target, "disconnect")
				return
			}
		}
	}
}

func readTerminalControls(
	conn webSocketConnection,
	input chan<- []byte,
	resizes *resizeQueue,
	result chan<- terminalClientResult,
) {
	for {
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if errors.Is(err, websocket.ErrReadLimit) {
				reportTerminalClientResult(result, terminalClientInvalidControl)
			} else {
				reportTerminalClientResult(result, terminalClientDisconnected)
			}
			return
		}
		if messageType != websocket.TextMessage {
			reportTerminalClientResult(result, terminalClientInvalidControl)
			return
		}
		var control ClientControl
		if err := json.Unmarshal(payload, &control); err != nil {
			reportTerminalClientResult(result, terminalClientInvalidControl)
			return
		}
		switch control.Type {
		case "input":
			select {
			case input <- []byte(control.Data):
			default:
				reportTerminalClientResult(result, terminalClientInvalidControl)
				return
			}
		case "resize":
			if control.Cols == 0 || control.Rows == 0 {
				reportTerminalClientResult(result, terminalClientInvalidControl)
				return
			}
			resizes.Update(remotecommand.TerminalSize{Width: control.Cols, Height: control.Rows})
		default:
			reportTerminalClientResult(result, terminalClientInvalidControl)
			return
		}
	}
}

func pumpTerminalInput(
	ctx context.Context,
	stdin *io.PipeWriter,
	input <-chan []byte,
	result chan<- terminalClientResult,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload := <-input:
			if _, err := stdin.Write(payload); err != nil {
				reportTerminalClientResult(result, terminalClientDisconnected)
				return
			}
		}
	}
}

func reportTerminalClientResult(result chan<- terminalClientResult, value terminalClientResult) {
	select {
	case result <- value:
	default:
	}
}

type serializedWebSocketWriter struct {
	conn webSocketWriteConnection
	now  func() time.Time
	mu   sync.Mutex
}

type webSocketWriteConnection interface {
	SetWriteDeadline(time.Time) error
	WriteMessage(int, []byte) error
	WriteJSON(any) error
	WriteControl(int, []byte, time.Time) error
}

type webSocketConnection interface {
	webSocketWriteConnection
	SetReadLimit(int64)
	SetReadDeadline(time.Time) error
	SetPongHandler(func(string) error)
	ReadMessage() (int, []byte, error)
	Close() error
}

func (w *serializedWebSocketWriter) WriteMessage(messageType int, payload []byte) error {
	return w.write(func(time.Time) error {
		return w.conn.WriteMessage(messageType, payload)
	})
}

func (w *serializedWebSocketWriter) WriteJSON(control ServerControl) error {
	return w.write(func(time.Time) error {
		return w.conn.WriteJSON(control)
	})
}

func (w *serializedWebSocketWriter) WritePing() error {
	return w.write(func(deadline time.Time) error {
		return w.conn.WriteControl(websocket.PingMessage, nil, deadline)
	})
}

func (w *serializedWebSocketWriter) write(write func(time.Time) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(terminalWriteTimeout)
	if err := w.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return write(deadline)
}

type websocketOutput struct {
	writer *serializedWebSocketWriter
}

func (w *websocketOutput) Write(payload []byte) (int, error) {
	copyOfPayload := append([]byte(nil), payload...)
	if err := w.writer.WriteMessage(websocket.BinaryMessage, copyOfPayload); err != nil {
		return 0, err
	}
	return len(payload), nil
}

type resizeQueue struct {
	mu      sync.Mutex
	updates chan remotecommand.TerminalSize
	closed  bool
}

func newResizeQueue() *resizeQueue {
	return &resizeQueue{updates: make(chan remotecommand.TerminalSize, 1)}
}

func (q *resizeQueue) Update(size remotecommand.TerminalSize) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	select {
	case <-q.updates:
	default:
	}
	q.updates <- size
}

func (q *resizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.updates
	if !ok {
		return nil
	}
	return &size
}

func (q *resizeQueue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	close(q.updates)
}

func logTerminalResult(log logr.Logger, target TerminalTarget, result string) {
	log.Info(
		"terminal session ended",
		"result", result,
		"namespace", target.Namespace,
		"sandbox", target.SandboxName,
		"pod", target.PodName,
		"container", target.Container,
	)
}
