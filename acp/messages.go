package acp

// messageLog groups one turn's agent_message_chunk text into the messages the
// agent sent. Chunks of one message are put back together; a chunk naming a
// different message starts the next one instead of being run into it. An agent
// that names no message gets every chunk of the turn as one message. Not safe
// for concurrent use: callers hold their own lock.
type messageLog struct {
	texts []string
	curID string
}

// add files one chunk under the message it belongs to. It returns the text of
// the message the chunk closes, when it starts a new message after an earlier
// one of the turn; the turn's last message is never closed here. An empty chunk
// opens nothing and closes nothing.
func (m *messageLog) add(text, messageID string) (closed string, ok bool) {
	if text == "" {
		return "", false
	}
	if len(m.texts) > 0 && messageID == m.curID {
		m.texts[len(m.texts)-1] += text
		return "", false
	}
	if n := len(m.texts); n > 0 {
		closed, ok = m.texts[n-1], true
	}
	m.texts = append(m.texts, text)
	m.curID = messageID
	return closed, ok
}

// last is the turn's reply: the text of its final message, "" when it sent none.
func (m *messageLog) last() string {
	if len(m.texts) == 0 {
		return ""
	}
	return m.texts[len(m.texts)-1]
}

// all copies every message of the turn, in order.
func (m *messageLog) all() []string {
	return append([]string(nil), m.texts...)
}
