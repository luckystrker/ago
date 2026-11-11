---------------------------- MODULE HubCore ----------------------------
(***************************************************************************)
(* Generic core of the network-hub delivery protocol, instantiated by the  *)
(* scenario modules HubDelivery and HubAccess (this file has no .cfg).     *)
(*                                                                         *)
(* At-least-once delivery with a persisted per-(agent,channel) cursor, WAL *)
(* replay on reconnect, causation-key idempotent posts, link drops, lost   *)
(* acks, hub crash/restart and client restart.                             *)
(*                                                                         *)
(* Durable (survives a hub crash): wal, cursor.                            *)
(* Hub-volatile: hubUp, vIdx (causation index), bound, ptr, hq, cq.        *)
(* Client-local: up, since (last id received per channel).                 *)
(* Ghosts (never read by actions): recvd, fb (fault budget).               *)
(*                                                                         *)
(* Envelope ids are WAL positions 1,2,3..; they grow in accept order like  *)
(* the sorted ids of the Python hub.                                       *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS
  Agents, Channels, Keys,    \* a Key = one logical post: (sender, causation key)
  KeyChan, KeySender, KeyAud, Part,
  MaxWal,                    \* = |Keys|; WAL capacity
  MaxFaults,                 \* total faults a run may contain
  Faults                     \* enabled fault kinds: "drop" "restart" "lose" "forge" "nack" "crash"

Ids == 1..MaxWal

Max(x, y) == IF x >= y THEN x ELSE y
Min(S)    == CHOOSE x \in S : \A y \in S : x <= y
Range(s)  == {s[n] : n \in 1..Len(s)}

VARIABLES
  wal,       \* durable: sequence of accepted keys; envelope id = position
  cursor,    \* durable: cursor[a][ch] = highest id acked by a in ch (0 = none)
  hubUp,     \* hub process alive
  vIdx,      \* volatile causation index: keys already accepted
  up,        \* client a has a live link
  bound,     \* the hub believes a live connection holds agent a
  hq,        \* hub->client link of a: FIFO of ids in flight
  cq,        \* client->hub receipt frames of a's connection, not yet processed
  ptr,       \* hub send pointer per (agent, channel): last id considered on the connection
  since,     \* client a's local high-water per channel (id it last received)
  recvd,     \* ghost: ids a's client actually received
  fb         \* ghost: faults used

vars == <<wal, cursor, hubUp, vIdx, up, bound, hq, cq, ptr, since, recvd, fb>>

Zero == [a \in Agents |-> [ch \in Channels |-> 0]]

ChOf(i) == KeyChan[wal[i]]
\* The spec: who is an addressee of key k.
Eligible(a, k) == /\ a \in Part[KeyChan[k]]
                  /\ a # KeySender[k]
                  /\ a \in KeyAud[k]
\* The hub's send filter (what the implementation checks).
HubSends(a, k) == /\ a \in Part[KeyChan[k]]
                  /\ a # KeySender[k]
                  /\ a \in KeyAud[k]
\* A receipt frame comes from the live connection and names the agent it holds.
Holds(x, f) == /\ f.rcpt = x
               /\ bound[x]

NextId(a, ch) ==
  LET S == {j \in (ptr[a][ch] + 1)..Len(wal) : ChOf(j) = ch}
  IN IF S = {} THEN 0 ELSE Min(S)

Init ==
  /\ wal = <<>>
  /\ cursor = Zero
  /\ hubUp = TRUE
  /\ vIdx = {}
  /\ up = [a \in Agents |-> FALSE]
  /\ bound = [a \in Agents |-> FALSE]
  /\ hq = [a \in Agents |-> <<>>]
  /\ cq = [a \in Agents |-> {}]
  /\ ptr = Zero
  /\ since = Zero
  /\ recvd = [a \in Agents |-> {}]
  /\ fb = 0

\* ---- posting (also the retry path: the same causation key may arrive again)
Post(k) ==
  /\ hubUp
  /\ IF k \in vIdx
       THEN UNCHANGED <<wal, vIdx>>
       ELSE /\ Len(wal) < MaxWal
            /\ wal' = Append(wal, k)
            /\ vIdx' = vIdx \cup {k}
  /\ UNCHANGED <<cursor, hubUp, up, bound, hq, cq, ptr, since, recvd, fb>>

\* ---- (re)connection: the hub binds the agent to the NEW connection (receipts still
\* queued from the replaced connection are no longer from the holder: dropped) and
\* replays strictly after max(persisted cursor, client since); the client re-acks its
\* high-water so the cursor catches up with acks that were lost.
Connect(a) ==
  /\ hubUp
  /\ ~up[a]
  /\ up' = [up EXCEPT ![a] = TRUE]
  /\ bound' = [bound EXCEPT ![a] = TRUE]
  /\ ptr' = [ptr EXCEPT ![a] = [ch \in Channels |-> Max(cursor[a][ch], since[a][ch])]]
  /\ cq' = [cq EXCEPT ![a] =
              {[kind |-> "ack", rcpt |-> a, ch |-> ch, id |-> since[a][ch]] :
                 ch \in {x \in Channels : since[a][x] > 0}}]
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, hq, since, recvd, fb>>

\* ---- hub sends the next WAL entry of channel ch to the connection holding a.
\* One ordered sender per (agent, channel): live dispatch and replay are the same loop.
Send(a, ch) ==
  /\ hubUp
  /\ bound[a]
  /\ NextId(a, ch) # 0
  /\ LET j == NextId(a, ch) IN
     /\ ptr' = [ptr EXCEPT ![a][ch] = j]
     /\ cursor' = cursor
     /\ IF HubSends(a, wal[j]) /\ up[a]
          THEN hq' = [hq EXCEPT ![a] = Append(@, j)]
          ELSE hq' = hq
  /\ UNCHANGED <<wal, hubUp, vIdx, up, bound, cq, since, recvd, fb>>

\* ---- client receives the head of its link and acks it
Receive(a) ==
  /\ up[a]
  /\ hq[a] # <<>>
  /\ LET i == Head(hq[a]) IN
     /\ hq' = [hq EXCEPT ![a] = Tail(@)]
     /\ recvd' = [recvd EXCEPT ![a] = @ \cup {i}]
     /\ since' = [since EXCEPT ![a][ChOf(i)] = Max(@, i)]
     /\ cq' = [cq EXCEPT ![a] = @ \cup
                {[kind |-> "ack", rcpt |-> a, ch |-> ChOf(i), id |-> i]}]
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, up, bound, ptr, fb>>

\* ---- client rejects the head: sends a nack and closes the link
Nack(a) ==
  /\ "nack" \in Faults
  /\ up[a]
  /\ hq[a] # <<>>
  /\ fb < MaxFaults
  /\ LET i == Head(hq[a]) IN
     cq' = [cq EXCEPT ![a] = @ \cup
              {[kind |-> "nack", rcpt |-> a, ch |-> ChOf(i), id |-> i]}]
  /\ up' = [up EXCEPT ![a] = FALSE]
  /\ hq' = [hq EXCEPT ![a] = <<>>]
  /\ fb' = fb + 1
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, bound, ptr, since, recvd>>

\* ---- hub processes one frame (any pending frame: handlers may run concurrently)
ProcessFrame(x, f) ==
  /\ hubUp
  /\ f \in cq[x]
  /\ cq' = [cq EXCEPT ![x] = @ \ {f}]
  /\ IF /\ f.kind = "ack"
        /\ Holds(x, f)
        /\ f.id > cursor[f.rcpt][f.ch]
       THEN cursor' = [cursor EXCEPT ![f.rcpt][f.ch] = f.id]
       ELSE UNCHANGED cursor
  /\ UNCHANGED <<wal, hubUp, vIdx, up, bound, hq, ptr, since, recvd, fb>>

\* ---- the hub notices that the link of a is gone
HubNotice(a) ==
  /\ hubUp
  /\ ~up[a]
  /\ bound[a]
  /\ bound' = [bound EXCEPT ![a] = FALSE]
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, up, hq, cq, ptr, since, recvd, fb>>

\* ---- faults (each costs one unit of the fault budget)
LinkDrop(a) ==
  /\ "drop" \in Faults
  /\ up[a]
  /\ fb < MaxFaults
  /\ up' = [up EXCEPT ![a] = FALSE]
  /\ hq' = [hq EXCEPT ![a] = <<>>]
  /\ fb' = fb + 1
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, bound, cq, ptr, since, recvd>>

\* the client process restarts: link gone, local high-water lost
ClientRestart(a) ==
  /\ "restart" \in Faults
  /\ fb < MaxFaults
  /\ up' = [up EXCEPT ![a] = FALSE]
  /\ hq' = [hq EXCEPT ![a] = <<>>]
  /\ since' = [since EXCEPT ![a] = [ch \in Channels |-> 0]]
  /\ fb' = fb + 1
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, bound, cq, ptr, recvd>>

\* a receipt is lost only with its connection: the link is reliable and ordered while up
LoseFrame(x, f) ==
  /\ "lose" \in Faults
  /\ ~up[x]
  /\ f \in cq[x]
  /\ fb < MaxFaults
  /\ cq' = [cq EXCEPT ![x] = @ \ {f}]
  /\ fb' = fb + 1
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, up, bound, hq, ptr, since, recvd>>

\* a connection of x sends an ack naming another agent r
ForgeAck(x, r, i) ==
  /\ "forge" \in Faults
  /\ up[x]
  /\ x # r
  /\ i \in 1..Len(wal)
  /\ fb < MaxFaults
  /\ cq' = [cq EXCEPT ![x] = @ \cup
             {[kind |-> "ack", rcpt |-> r, ch |-> ChOf(i), id |-> i]}]
  /\ fb' = fb + 1
  /\ UNCHANGED <<wal, cursor, hubUp, vIdx, up, bound, hq, ptr, since, recvd>>

Crash ==
  /\ "crash" \in Faults
  /\ hubUp
  /\ fb < MaxFaults
  /\ hubUp' = FALSE
  /\ vIdx' = {}
  /\ up' = [a \in Agents |-> FALSE]
  /\ bound' = [a \in Agents |-> FALSE]
  /\ hq' = [a \in Agents |-> <<>>]
  /\ cq' = [a \in Agents |-> {}]
  /\ ptr' = Zero
  /\ fb' = fb + 1
  /\ UNCHANGED <<wal, cursor, since, recvd>>

\* hydrate: the causation index is rebuilt from the WAL
Restart ==
  /\ ~hubUp
  /\ hubUp' = TRUE
  /\ vIdx' = Range(wal)
  /\ UNCHANGED <<wal, cursor, up, bound, hq, cq, ptr, since, recvd, fb>>

Next ==
  \/ \E k \in Keys : Post(k)
  \/ \E a \in Agents : Connect(a) \/ Receive(a) \/ Nack(a) \/ HubNotice(a) \/ LinkDrop(a) \/ ClientRestart(a)
  \/ \E a \in Agents, ch \in Channels : Send(a, ch)
  \/ \E x \in Agents : \E f \in cq[x] : ProcessFrame(x, f) \/ LoseFrame(x, f)
  \/ \E x, r \in Agents, i \in Ids : ForgeAck(x, r, i)
  \/ Crash
  \/ Restart

Fairness ==
  /\ WF_vars(Restart)
  /\ \A a \in Agents : /\ WF_vars(Connect(a))
                       /\ WF_vars(Receive(a))
                       /\ WF_vars(HubNotice(a))
                       /\ WF_vars(\E f \in cq[a] : ProcessFrame(a, f))
  /\ \A a \in Agents, ch \in Channels : WF_vars(Send(a, ch))

Spec == Init /\ [][Next]_vars /\ Fairness

(***************************************************************************)
(* Invariants and properties                                               *)
(***************************************************************************)
TypeOK ==
  /\ hubUp \in BOOLEAN
  /\ Len(wal) <= MaxWal
  /\ \A a \in Agents, ch \in Channels : cursor[a][ch] \in 0..MaxWal
  /\ \A a \in Agents : hq[a] \in Seq(Ids)

\* (4) nothing reaches a non-participant, a non-audience member or the sender:
\* every id received, or in flight on a link, is addressed to that agent.
NoLeak ==
  \A a \in Agents : \A i \in recvd[a] \cup Range(hq[a]) : i <= Len(wal) /\ Eligible(a, wal[i])

\* (4) the cursor only holds ids the agent itself received in that channel:
\* a foreign/forged ack, or an ack in another channel, cannot have moved it.
CursorBacked ==
  \A a \in Agents, ch \in Channels :
    cursor[a][ch] = 0 \/ (cursor[a][ch] \in recvd[a] /\ ChOf(cursor[a][ch]) = ch)

\* (3) a post retried under the same causation key is accepted once.
NoDoubleAccept ==
  \A p, q \in 1..Len(wal) : p # q => wal[p] # wal[q]

\* (5) after a restart the causation index equals the keys in the WAL.
IndexFaithful == hubUp => vIdx = Range(wal)

\* (2)+(3) every send (live or replay) names an id strictly above the persisted cursor
\* and the client's high-water of its channel at that moment: nothing is delivered
\* after its ack is persisted. (An ack of an earlier delivery may still land later.)
ReplayStrictlyAfter ==
  [][\A a \in Agents :
       Len(hq'[a]) > Len(hq[a]) =>
         LET j == hq'[a][Len(hq'[a])]
         IN /\ j > cursor[a][ChOf(j)]
            /\ j > since[a][ChOf(j)]]_vars

\* (2) in-flight deliveries of one channel are in WAL order.
OrderedDelivery ==
  \A a \in Agents : \A p, q \in 1..Len(hq[a]) :
    (p < q /\ ChOf(hq[a][p]) = ChOf(hq[a][q])) => hq[a][p] < hq[a][q]

\* (2) the persisted cursor only moves forward.
CursorMonotonic ==
  [][\A a \in Agents, ch \in Channels : cursor'[a][ch] >= cursor[a][ch]]_vars

\* (1) every accepted envelope is eventually received and acked by every addressee.
NothingLost ==
  \A a \in Agents : \A i \in Ids :
    (i <= Len(wal) /\ Eligible(a, wal[i]))
      ~> (i \in recvd[a] /\ cursor[a][KeyChan[wal[i]]] >= i)

\* WITNESS, not an invariant (it is not listed in any .cfg): "nothing in flight was already
\* received". TLC is expected to VIOLATE it (client restart / lost receipt => redelivery), which
\* shows the bounded-duplicates scenario is reachable. Check: add it to INVARIANTS of HubDelivery.cfg.
NoDupWitness ==
  \A a \in Agents : \A n \in 1..Len(hq[a]) : hq[a][n] \notin recvd[a]

=============================================================================
