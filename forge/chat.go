package forge

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Marb-AI/forge/config"
	"github.com/Marb-AI/forge/internal/agentproto"
	"github.com/Marb-AI/forge/internal/sshx"
)

// Chatting with a workspace's Claude.
//
// Two operations, because a turn is two things that happen at different times
// and to different people: somebody asks, and then anybody — the same window, a
// second one, or a phone that has been in a tunnel for twenty minutes — reads
// what came back. Splitting them is what makes the second one repeatable.
//
// Neither of these knows what Claude Code is. The turn runs on the host, where
// the login and the workspace are (see internal/agent/chat.go); what is here is
// the two ends of a pipe.

// ChatSend asks a workspace's Claude something and returns the id of the turn
// that will answer.
//
// It returns as soon as the turn is running, not when it is finished: an answer
// takes as long as it takes, and a caller that waited for it would hold a
// request open across the whole of it — which on the one connection a phone has
// is the difference between a chat and a timeout. What the caller does next is
// read the turn.
func ChatSend(workspace, prompt string) (string, error) {
	host, err := chatHost(workspace)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(prompt) == "" {
		return "", fmt.Errorf("nothing to send")
	}
	var res agentproto.ChatTurn
	if err := callAgentWith(host, strings.NewReader(prompt), &res,
		"claude-chat-send", "-name", workspace); err != nil {
		return "", err
	}
	if res.Turn == "" {
		return "", fmt.Errorf("the host started a turn and did not say which")
	}
	return res.Turn, nil
}

// ChatTail writes a turn's stream to w, starting offset bytes in, and returns
// when the turn is over.
//
// The bytes are Claude Code's own stream-json, unaltered and unbuffered by
// anything here. That is deliberate: this is the middle of a pipe, and a middle
// that parses is a middle that has opinions about a format somebody else
// versions. The one caller that needs to understand the stream — the browser —
// is where understanding it belongs.
//
// offset is how much of the turn the caller already has, which is the whole of
// what it must remember to come back to one. Zero reads it from the beginning,
// which is what makes a reconnect and a first look the same request.
func ChatTail(workspace, turn string, offset int64, w io.Writer) error {
	host, err := chatHost(workspace)
	if err != nil {
		return err
	}
	if offset < 0 {
		return fmt.Errorf("offset %d is before the start of the turn", offset)
	}
	remote := agentCommand(host, "claude-chat-tail",
		"-name", workspace, "-turn", turn, "-offset", strconv.FormatInt(offset, 10))

	// Straight down the wire and out to w, with no buffer in between: a chat that
	// arrives a paragraph at a time is a chat, and one that arrives when the turn
	// ends is a form submission.
	//
	// Stderr is kept rather than let out to this process's, which is where every
	// other remote failure is explained. It has to be: the agent's tail command
	// carries Claude Code's stream on stdout and so says everything it has to say
	// on stderr — "no turn X in this workspace" among it — and that is the one
	// sentence worth putting in front of whoever is waiting for an answer. Left to
	// the daemon's log it would reach nobody who could act on it.
	var why boundedBuffer
	return explain(sshx.AdminTarget(host).Pipe(nil, w, &why, remote...), &why)
}

// explain prefers the far end's own account of a failure to the transport's.
//
// "ssh exited 1" is true and useless; "no turn 20260805T142530.123456789 in
// \"ws\" on this host" is what somebody can act on, and it is on stderr because
// stdout is carrying the conversation. When the far end said nothing — a
// connection that never got there — the transport's error is all there is, and
// it is returned unchanged.
//
// Nothing is said about a success, however talkative it was: a warning is not a
// failure, and a caller that turned one into an error would end a turn that went
// perfectly well.
func explain(err error, why *boundedBuffer) error {
	if err == nil {
		return nil
	}
	if said := strings.TrimSpace(why.String()); said != "" {
		return fmt.Errorf("%s", said)
	}
	return err
}

// boundedBuffer keeps the first of what it is given and silently drops the rest.
//
// What it holds is an explanation destined for a browser, and the far end is a
// remote process that can be made to say anything at any length. A sentence is
// what this is for.
type boundedBuffer struct {
	b strings.Builder
}

// chatWhyLimit is how much of a failure's explanation survives. Long enough for
// the agent's own sentences and a shell's complaint under them, short enough
// that nothing is being stored.
const chatWhyLimit = 4 << 10

func (s *boundedBuffer) Write(p []byte) (int, error) {
	if room := chatWhyLimit - s.b.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		s.b.Write(p)
	}
	// The whole write is reported as taken: the far end is not being asked to
	// resend anything, and a short write would end the stream it belongs to.
	return len(p), nil
}

func (s *boundedBuffer) Len() int       { return s.b.Len() }
func (s *boundedBuffer) String() string { return s.b.String() }

// ChatHistory writes the last n turns of a workspace's conversation to w,
// oldest first, and returns when it has.
//
// One call rather than a listing the caller then fetches turn by turn: a page
// coming back is one round trip over SSH instead of twenty, and the transcript
// arrives in the order it happened without anything here sorting it.
//
// It is the same stream as a live turn with one addition — a line of Forge's own
// introducing each turn with the prompt that started it, which exists nowhere in
// Claude Code's output because it went in on stdin. A reader can therefore treat
// history and a turn in flight identically, which is the point: coming back to a
// conversation and joining one are the same code.
func ChatHistory(workspace string, turns int, w io.Writer) error {
	host, err := chatHost(workspace)
	if err != nil {
		return err
	}
	if turns <= 0 {
		return fmt.Errorf("asked for %d turns", turns)
	}
	remote := agentCommand(host, "claude-chat-history",
		"-name", workspace, "-turns", strconv.Itoa(turns))

	var why boundedBuffer
	return explain(sshx.AdminTarget(host).Pipe(nil, w, &why, remote...), &why)
}

// chatHost is the server a workspace lives on. Same refusal as everywhere else
// that takes a workspace name: Forge acts on what its own config records, so a
// workspace this client did not create is unknown however plainly it exists.
func chatHost(workspace string) (*config.Host, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	host := cfg.HostFor(workspace)
	if host == nil {
		return nil, fmt.Errorf("unknown workspace %q — not created by this client", workspace)
	}
	return host, nil
}
