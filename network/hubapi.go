package network

import "github.com/luckystrker/ago/task"

// hubAPI is the slice of the hub that AgentClient, HumanClient and ClientTask
// talk to. *Hub implements it directly (in-process); remoteHub implements it
// by control-plane requests over a transport.Link, so the clients run
// unchanged against either.
type hubAPI interface {
	Register(p Passport, r *Rule) (Passport, error)
	SetResume(r Resume) error
	Attach(agentID string, fn Notifier) error
	Unregister(agentID string) error
	Passport(nameOrID string) (Passport, error)
	RuleOf(agentID string) (Rule, error)
	Send(env Envelope) (Envelope, error)
	CreateChannel(manifest ChannelManifest, participants []string) (*ChannelHandle, error)
	Channel(channelID string) (*ChannelHandle, error)
	CloseChannel(channelID, reason string) error
	AckInbox(agentID string, n int)
	bookInbox(agentID string)
	FindEnvelopeByCausation(channelID, senderID, causationID string) (Envelope, bool)
	ReportTurnFailure(channelID, agentID, envelopeID string, err error)
	AddResumeExample(agentID string, ex ResumeExample) error
	SetRule(agentID string, r Rule) error
	PendingTurnsFor(agentID string) []PendingTurn
	envelopeByID(channelID, envelopeID string) (Envelope, bool)
	Task(taskID string) (task.Metadata, error)

	// Reads the client tools (peers, channels, tasks, context, delegate) use.
	Registrations() []Passport
	ResumeOf(agentID string) (Resume, error)
	ListChannels() []*ChannelHandle
	Tasks(owner string, activeOnly bool) []task.Metadata
	ReadWAL(channelID string) ([]Envelope, error)
}
