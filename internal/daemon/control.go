package daemon

import (
	"encoding/json"
	"net"
	"time"
)

const protocolVersion = 1

// handshakeMsg is the single supervisor-to-CLI message reporting a successful
// start or a first-start failure.
type handshakeMsg struct {
	V             int    `json:"v"`
	Token         string `json:"token"`
	Event         string `json:"event"` // ready | error
	Message       string `json:"message,omitempty"`
	ControlAddr   string `json:"controlAddr,omitempty"`
	ControlToken  string `json:"controlToken,omitempty"`
	SupervisorPid int    `json:"supervisorPid,omitempty"`
	WorkerPid     int    `json:"workerPid,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
}

// controlRequest is sent by status/stop commands to the supervisor control
// listener. Token authenticates the caller.
type controlRequest struct {
	V       int    `json:"v"`
	Token   string `json:"token"`
	Command string `json:"command"` // status | stop
}

type controlResponse struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	Status        string `json:"status,omitempty"` // starting|running|backoff|failed|stopping|stopped
	Port          int    `json:"port"`
	SupervisorPid int    `json:"supervisorPid"`
	WorkerPid     int    `json:"workerPid"`
	Restarts      int    `json:"restarts"`
	MaxRestarts   int    `json:"maxRestarts"`
	LastExit      string `json:"lastExit,omitempty"`
	NextRestartMs int64  `json:"nextRestartMs,omitempty"`
	LogPath       string `json:"logPath,omitempty"`
}

// mgmtMsg carries supervisor-worker management traffic. Token authenticates
// the hello message that opens each connection.
type mgmtMsg struct {
	V       int    `json:"v"`
	Token   string `json:"token,omitempty"`
	Event   string `json:"event,omitempty"`   // hello | ready | failed
	Command string `json:"command,omitempty"` // stop
	Port    int    `json:"port,omitempty"`
	Message string `json:"message,omitempty"`
}

type discovery struct {
	V           int    `json:"v"`
	ControlAddr string `json:"controlAddr"`
	Token       string `json:"token"`
	Port        int    `json:"port"`
}

// jsonConn exchanges newline-delimited JSON over one TCP connection. A single
// decoder is kept for the lifetime of the connection so consecutive reads
// never lose buffered bytes.
type jsonConn struct {
	conn    net.Conn
	decoder *json.Decoder
}

func newJSONConn(conn net.Conn) *jsonConn {
	return &jsonConn{conn: conn, decoder: json.NewDecoder(conn)}
}

func (c *jsonConn) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	_, err = c.conn.Write(append(data, '\n'))
	return err
}

// read decodes one JSON message, bounded by timeout; a non-positive timeout
// blocks until a message arrives or the connection closes.
func (c *jsonConn) read(value any, timeout time.Duration) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	return c.decoder.Decode(value)
}

func (c *jsonConn) close() error {
	return c.conn.Close()
}
