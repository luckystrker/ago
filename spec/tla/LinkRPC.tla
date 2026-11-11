------------------------------- MODULE LinkRPC -------------------------------
(***************************************************************************)
(* Request/response over the hub link (HubClient._pending): requests carry *)
(* a request_id, the hub answers with the same id, the client matches the  *)
(* response to the waiting caller by id. The link may drop: all waiters    *)
(* fail at once and the client reconnects on a fresh connection, but frames*)
(* of the old connection may still be in flight and arrive late, and the   *)
(* transport may deliver a frame twice.                                    *)
(* Wave-4 invariant 6.                                                     *)
(*                                                                         *)
(* A request has a hidden identity `ser` (globally unique) and a wire id   *)
(* `rid`. The protocol must make rid unambiguous: here rid is a global     *)
(* monotonic counter (the Python make_id() is unique too), NOT a per-      *)
(* connection counter.                                                     *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANTS MaxReq, MaxFaults

VARIABLES
  nextSer,  \* next unique request serial (1..MaxReq+1)
  up,       \* client link alive
  ep,       \* connection epoch (number of connects so far)
  ctr,      \* requests sent on the current connection
  pend,     \* waiting callers: [rid, ser, ep]
  rq,       \* request frames in flight to the hub: [rid, ser, ep]
  rs,       \* response frames in flight to the client: [rid, ser, ep] (echo of the request)
  ans,      \* ghost: log of satisfied waiters: [w |-> <<ser,ep>> of waiter, f |-> <<ser,ep>> of response]
  fb        \* ghost: faults used

vars == <<nextSer, up, ep, ctr, pend, rq, rs, ans, fb>>

NewRid == nextSer      \* a unique id for the whole process lifetime

Init ==
  /\ nextSer = 1
  /\ up = TRUE
  /\ ep = 1
  /\ ctr = 0
  /\ pend = {}
  /\ rq = {}
  /\ rs = {}
  /\ ans = <<>>
  /\ fb = 0

Request ==
  /\ up
  /\ nextSer <= MaxReq
  /\ LET fr == [rid |-> NewRid, ser |-> nextSer, ep |-> ep] IN
     /\ pend' = pend \cup {fr}
     /\ rq' = rq \cup {fr}
  /\ nextSer' = nextSer + 1
  /\ ctr' = ctr + 1
  /\ UNCHANGED <<up, ep, rs, ans, fb>>

\* The hub answers a request frame (it may do so after the connection is gone).
HubServe(f) ==
  /\ f \in rq
  /\ rq' = rq \ {f}
  /\ rs' = rs \cup {f}
  /\ UNCHANGED <<nextSer, up, ep, ctr, pend, ans, fb>>

\* The client reads a response frame from its current link and routes it by id.
Match(f) == {p \in pend : p.rid = f.rid}

\* keep = TRUE: the transport delivers the frame and keeps a copy that is delivered
\* again (costs one fault).
Deliver(f, keep) ==
  /\ f \in rs
  /\ up
  /\ keep => fb < MaxFaults
  /\ \/ /\ Match(f) # {}
        /\ \E p \in Match(f) :
             /\ pend' = pend \ {p}
             /\ ans' = Append(ans, [w |-> <<p.ser, p.ep>>, f |-> <<f.ser, f.ep>>])
     \/ /\ Match(f) = {}
        /\ UNCHANGED <<pend, ans>>
  /\ rs' = IF keep THEN rs ELSE rs \ {f}
  /\ fb' = IF keep THEN fb + 1 ELSE fb
  /\ UNCHANGED <<nextSer, up, ep, ctr, rq>>

LinkDrop ==
  /\ up
  /\ fb < MaxFaults
  /\ up' = FALSE
  /\ pend' = {}            \* every waiter fails with "connection closed"
  /\ fb' = fb + 1
  /\ UNCHANGED <<nextSer, ep, ctr, rq, rs, ans>>

Reconnect ==
  /\ ~up
  /\ up' = TRUE
  /\ ep' = ep + 1
  /\ ctr' = 0
  /\ UNCHANGED <<nextSer, pend, rq, rs, ans, fb>>

\* Legitimate end: no more requests, nothing in flight.
Done ==
  /\ nextSer > MaxReq
  /\ pend = {} /\ rq = {} /\ rs = {}
  /\ UNCHANGED vars

Next ==
  \/ Request
  \/ \E f \in rq : HubServe(f)
  \/ \E f \in rs, keep \in BOOLEAN : Deliver(f, keep)
  \/ LinkDrop
  \/ Reconnect
  \/ Done

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ nextSer \in 1..(MaxReq + 1)
  /\ up \in BOOLEAN
  /\ ep \in 1..(MaxFaults + 1)

\* (6) each request gets at most one response.
AtMostOneResponse ==
  \A p, q \in 1..Len(ans) : p # q => ans[p].w # ans[q].w

\* (6) the response that satisfies a request is that request's own.
ResponseIsOwn ==
  \A n \in 1..Len(ans) : ans[n].w[1] = ans[n].f[1]

\* (6) a response from an old connection never satisfies a request of a newer one.
NoStaleSatisfy ==
  \A n \in 1..Len(ans) : ans[n].w[2] = ans[n].f[2]

=============================================================================
