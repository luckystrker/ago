--------------------------- MODULE A2AResubscribe ---------------------------
(***************************************************************************)
(* A2A client streaming a remote task, reconnecting by resubscribing.      *)
(* The server's task moves submitted -> working -> completed|failed, and   *)
(* while working it accumulates progress (artifact chunks n = 0..MaxN).    *)
(* It keeps moving while the client is disconnected. A stream carries the  *)
(* events of changes that happen while it is open; a resubscribe opens     *)
(* with the CURRENT task (a snapshot: state and progress), then continues; *)
(* the server closes a stream right after the terminal event.              *)
(* Wave-4 invariant 7.                                                     *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS MaxDrops,   \* how many times the stream may break in one run
          MaxN        \* progress chunks a task may produce

States   == {"submitted", "working", "completed", "failed"}
Terminal == {"completed", "failed"}
Rank == [s \in States |-> IF s = "submitted" THEN 1 ELSE IF s = "working" THEN 2 ELSE 3]
Tasks == [st : States, n : 0..MaxN]

VARIABLES
  srv,    \* server's current task: [st, n]
  sub,    \* the server holds an open stream to the client
  q,      \* events in flight on the open stream (FIFO); each event is a task snapshot
  view,   \* client's view of the task ([st, n]; st = "none" until the first event)
  drops   \* ghost: stream breaks used

vars == <<srv, sub, q, view, drops>>

NoView == [st |-> "none", n |-> 0]
ViewRank(v) == IF v.st = "none" THEN 0 ELSE Rank[v.st] * (MaxN + 1) + v.n

Init ==
  /\ srv = [st |-> "submitted", n |-> 0]
  /\ sub = FALSE
  /\ q = <<>>
  /\ view = NoView
  /\ drops = 0

\* The task changes; every open stream gets the event; a terminal event closes the stream.
Emit(t2) ==
  /\ srv' = t2
  /\ IF sub THEN q' = Append(q, t2) ELSE q' = q
  /\ sub' = IF t2.st \in Terminal THEN FALSE ELSE sub
  /\ UNCHANGED <<view, drops>>

Advance ==
  /\ srv.st = "submitted"
  /\ Emit([st |-> "working", n |-> srv.n])

Progress ==
  /\ srv.st = "working" /\ srv.n < MaxN
  /\ Emit([st |-> "working", n |-> srv.n + 1])

Finish(t) ==
  /\ srv.st = "working"
  /\ t \in Terminal
  /\ Emit([st |-> t, n |-> srv.n])

\* (Re)subscribe: the stream opens with the current task snapshot.
Subscribe ==
  /\ ~sub
  /\ view.st \notin Terminal
  /\ q' = <<srv>>
  /\ sub' = (srv.st \notin Terminal)
  /\ UNCHANGED <<srv, view, drops>>

\* The client applies the next event of its stream.
Receive ==
  /\ q # <<>>
  /\ view' = Head(q)
  /\ q' = Tail(q)
  /\ UNCHANGED <<srv, sub, drops>>

\* The connection breaks: the stream and its unread events are lost.
Drop ==
  /\ drops < MaxDrops
  /\ sub \/ q # <<>>
  /\ sub' = FALSE
  /\ q' = <<>>
  /\ drops' = drops + 1
  /\ UNCHANGED <<srv, view>>

\* Legitimate end: the terminal state is observed and nothing is left.
Done ==
  /\ view.st \in Terminal
  /\ q = <<>>
  /\ UNCHANGED vars

Next == Advance \/ Progress \/ (\E t \in Terminal : Finish(t)) \/ Subscribe \/ Receive \/ Drop \/ Done

Fairness ==
  /\ WF_vars(Advance) /\ WF_vars(Progress) /\ WF_vars(\E t \in Terminal : Finish(t))
  /\ WF_vars(Subscribe) /\ WF_vars(Receive)

Spec == Init /\ [][Next]_vars /\ Fairness

TypeOK ==
  /\ srv \in Tasks
  /\ view \in Tasks \cup {NoView}
  /\ q \in Seq(Tasks)

\* (7) no events after terminal: a terminal event is the last one of any stream.
TerminalIsLast ==
  \A n \in 1..Len(q) : q[n].st \in Terminal => n = Len(q)

\* (7) no events after terminal: the stream is closed once the task is terminal,
\* and a terminal task never changes again.
StreamClosedAtTerminal == srv.st \in Terminal => ~sub
TerminalIsFinal == [][(srv.st \in Terminal) => (srv' = srv)]_vars

\* (7) the client's view never moves back in the lifecycle (and never leaves a terminal state).
ViewMonotone ==
  [][ViewRank(view') >= ViewRank(view)]_vars

\* (7) after reconnecting, the client's view converges to the server's task.
Converges == <>[](view = srv)

\* (7) the terminal state is always observed.
TerminalObserved == (srv.st \in Terminal) ~> (view.st \in Terminal)

=============================================================================
