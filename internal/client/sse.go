package client

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// frame is one server-sent event: its name and its decoded payload.
type frame struct {
	Event string
	Data  []byte
}

// event is the payload the router sends on every client-facing frame.
//
// It mirrors httpapi's sseEvent rather than importing it: this package is a
// CLIENT of the HTTP surface and must decode what arrives on the wire, not share
// a struct with the server. Sharing one would hide a wire-format change behind a
// compiler that is happy because both sides moved together.
type event struct {
	JobID  string `json:"job_id"`
	Units  int    `json:"units"`
	Reason string `json:"reason"`
	Label  string `json:"label"`
}

// backlogPayload is the `backlog` frame: what is already waiting on connect.
type backlogPayload struct {
	Jobs []struct {
		JobID string `json:"job_id"`
		Units int    `json:"units"`
	} `json:"jobs"`
}

// readFrames parses an SSE stream and sends each complete frame on out.
//
// ⚠ IT REASSEMBLES, rather than reading a line and assuming it is an event. An
// SSE frame is `event:` then `data:` then a BLANK LINE, and the transport gives
// no guarantee about how those land in reads: two frames can arrive in one TCP
// segment and one frame can span two. A reader that treats each read as a frame
// drops events under exactly the load that makes them matter.
//
// It closes out when the stream ends, so a caller ranging over it learns the
// connection died rather than blocking forever.
func readFrames(r io.Reader, out chan<- frame) {
	defer close(out)

	sc := bufio.NewScanner(r)
	// A result can be large and the frames carry counts rather than content, but
	// a generous buffer costs nothing and a truncated frame is a silent drop.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var current frame
	for sc.Scan() {
		line := sc.Text()

		switch {
		case line == "":
			// The blank line terminates a frame. An empty name means we saw only
			// a comment or a stray data line, and there is nothing to deliver.
			if current.Event != "" {
				out <- current
			}
			current = frame{}

		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")

		case strings.HasPrefix(line, "data: "):
			current.Data = []byte(strings.TrimPrefix(line, "data: "))

		case strings.HasPrefix(line, ":"):
			// A comment, used by some servers as a keepalive. Ignored.
		}
	}
}

// decode unmarshals a frame's payload.
func (f frame) decode(v any) error {
	if len(f.Data) == 0 {
		return nil
	}
	return json.Unmarshal(f.Data, v)
}
