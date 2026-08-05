package forge

import (
	"fmt"
	"io"
	"os"
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
	// The agent keeps its own complaints off stdout for exactly this reason, so
	// stderr is where they are, and this process's is where they go — the same
	// place every other remote failure is explained.
	return sshx.AdminTarget(host).Pipe(nil, w, os.Stderr, remote...)
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
