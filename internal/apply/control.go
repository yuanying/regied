package apply

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

type ControlVerb string

const (
	ControlTrial   ControlVerb = "trial"
	ControlConfirm ControlVerb = "confirm"
	ControlCancel  ControlVerb = "cancel"
)

type ControlRequest struct {
	Verb        ControlVerb `json:"verb"`
	Declaration []byte      `json:"declaration,omitempty"`
	Source      string      `json:"source,omitempty"`
	Deadline    time.Time   `json:"deadline,omitempty"`
	reply       chan ControlResponse
}

func (r ControlRequest) Reply(response ControlResponse) { r.reply <- response }

type ControlResponse struct {
	Revision string `json:"revision,omitempty"`
	State    State  `json:"state,omitempty"`
	Error    string `json:"error,omitempty"`
}

type Control interface {
	Listen(context.Context, string, fs.FileMode) (<-chan ControlRequest, func() error, error)
	Do(context.Context, string, ControlRequest) (ControlResponse, error)
}

type OSControl struct{}

func validControlVerb(verb ControlVerb) bool {
	return verb == ControlTrial || verb == ControlConfirm || verb == ControlCancel
}

func (OSControl) Listen(ctx context.Context, socket string, mode fs.FileMode) (<-chan ControlRequest, func() error, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o750); err != nil {
		return nil, nil, err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, nil, fmt.Errorf("refusing to replace non-socket path %s", socket)
		}
		if err := os.Remove(socket); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(socket, mode); err != nil {
		listener.Close()
		return nil, nil, err
	}
	out := make(chan ControlRequest)
	go func() {
		defer close(out)
		go func() { <-ctx.Done(); listener.Close() }()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go serveControlConnection(ctx, conn, out)
		}
	}()
	closeControl := func() error {
		err := listener.Close()
		_ = os.Remove(socket)
		return err
	}
	return out, closeControl, nil
}

func serveControlConnection(ctx context.Context, conn net.Conn, out chan<- ControlRequest) {
	defer conn.Close()
	var request ControlRequest
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil {
		_ = json.NewEncoder(conn).Encode(ControlResponse{Error: err.Error()})
		return
	}
	if !validControlVerb(request.Verb) {
		_ = json.NewEncoder(conn).Encode(ControlResponse{Error: fmt.Sprintf("unsupported control verb %q", request.Verb)})
		return
	}
	request.reply = make(chan ControlResponse, 1)
	select {
	case out <- request:
	case <-ctx.Done():
		return
	}
	select {
	case response := <-request.reply:
		_ = json.NewEncoder(conn).Encode(response)
	case <-ctx.Done():
	}
}

func (OSControl) Do(ctx context.Context, socket string, request ControlRequest) (ControlResponse, error) {
	if !validControlVerb(request.Verb) {
		return ControlResponse{}, fmt.Errorf("unsupported control verb %q", request.Verb)
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", socket)
	if err != nil {
		return ControlResponse{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return ControlResponse{}, err
	}
	var response ControlResponse
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&response); err != nil {
		return ControlResponse{}, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}
