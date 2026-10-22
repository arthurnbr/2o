package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

const MaxHeader = 16 << 10

type Message struct {
	Op    string `json:"op,omitempty"`
	Name  string `json:"name,omitempty"`
	URL   string `json:"url,omitempty"`
	Title string `json:"title,omitempty"`
	Body  string `json:"body,omitempty"`
	Size  int64  `json:"size,omitempty"`
	Mode  uint32 `json:"mode,omitempty"`
	Hash  string `json:"hash,omitempty"`
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`

	Desktop string   `json:"desktop,omitempty"` // desktop identity label (user@host)
	Version string   `json:"version,omitempty"` // 2o release of the sender
	Line    int      `json:"line,omitempty"`    // edit FILE:LINE
	Path    string   `json:"path,omitempty"`    // edit recovery copy on the desktop
	Hints   []string `json:"hints,omitempty"`   // userError hints
	Code    int      `json:"code,omitempty"`    // userError exit code
	Checks  []Check  `json:"checks,omitempty"`  // doctor reply
}

// Check is one line of a `2o doctor` report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

func Write(w io.Writer, m Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxHeader {
		return errors.New("header too large")
	}
	var h [4]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(b)))
	if n, err := w.Write(h[:]); err != nil {
		return err
	} else if n != len(h) {
		return io.ErrShortWrite
	}
	if n, err := w.Write(b); err != nil {
		return err
	} else if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}
func Read(r io.Reader) (Message, error) {
	var m Message
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return m, err
	}
	n := binary.BigEndian.Uint32(h[:])
	if n == 0 || n > MaxHeader {
		return m, errors.New("invalid header size")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return m, err
	}
	err := json.Unmarshal(b, &m)
	return m, err
}
