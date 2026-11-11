------------------------------ MODULE HubRestart ------------------------------
(***************************************************************************)
(* Hub persistence across a crash: registry (passports), inbox cursors and *)
(* the WAL are durable; their in-memory copies and the causation index are *)
(* volatile and rebuilt by hydrate. Every write is two steps (the crash    *)
(* can land between them). A crash may also hit hydrate itself, and        *)
(* hydrate may be run again on a live hub. Wave-4 invariant 5.             *)
(*                                                                         *)
(* One channel; cursor values 0..MaxId; two keys.                          *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANT MaxFaults

Agents == {"a", "b"}
Keys   == {"k1", "k2"}
MaxId  == 2
ZeroC  == [a \in Agents |-> 0]
NoPend == [op |-> "none", a |-> "a"]
Range(s) == {s[n] : n \in 1..Len(s)}

VARIABLES
  dReg, dCur, dWal,      \* durable: registered agents, cursor per agent, accepted keys (in order)
  vReg, vCur, vIdx,      \* volatile copies (cursor, registry) and the causation index
  phase,                 \* "up" | "down" | "hydrating"
  hp,                    \* hydrate progress: WAL entries folded into vIdx
  pend,                  \* the write whose second step has not run yet, or "none"
  dead,                  \* ghost: agents deregistered and not registered since
  fb                     \* ghost: faults used

vars == <<dReg, dCur, dWal, vReg, vCur, vIdx, phase, hp, pend, dead, fb>>

Init ==
  /\ dReg = {} /\ dCur = ZeroC /\ dWal = <<>>
  /\ vReg = {} /\ vCur = ZeroC /\ vIdx = {}
  /\ phase = "up" /\ hp = 0 /\ pend = NoPend /\ dead = {} /\ fb = 0

Serving == phase = "up" /\ pend = NoPend

\* register: persist first, then memory
Register(a) ==
  /\ Serving /\ a \notin dReg
  /\ dReg' = dReg \cup {a}
  /\ pend' = [op |-> "reg", a |-> a]
  /\ dead' = dead \ {a}
  /\ UNCHANGED <<dCur, dWal, vReg, vCur, vIdx, phase, hp, fb>>

\* deregister: the passport and the agent's cursor are deleted
Deregister(a) ==
  /\ Serving /\ a \in dReg
  /\ dReg' = dReg \ {a}
  /\ dCur' = [dCur EXCEPT ![a] = 0]
  /\ pend' = [op |-> "dereg", a |-> a]
  /\ dead' = dead \cup {a}
  /\ UNCHANGED <<dWal, vReg, vCur, vIdx, phase, hp, fb>>

\* ack: memory first, then persist the cursor (as _advance_cursor does)
Ack(a, i) ==
  /\ Serving /\ a \in vReg /\ i > vCur[a]
  /\ vCur' = [vCur EXCEPT ![a] = i]
  /\ pend' = [op |-> "ack", a |-> a]
  /\ UNCHANGED <<dReg, dCur, dWal, vReg, vIdx, phase, hp, dead, fb>>

\* post: append to the WAL, index the key
Post(k) ==
  /\ Serving
  /\ IF k \in vIdx
       THEN UNCHANGED <<dWal, vIdx>>
       ELSE /\ Len(dWal) < Cardinality(Keys)
            /\ dWal' = Append(dWal, k)
            /\ vIdx' = vIdx \cup {k}
  /\ UNCHANGED <<dReg, dCur, vReg, vCur, phase, hp, pend, dead, fb>>

\* the second step of a pending write
Complete ==
  /\ phase = "up" /\ pend.op # "none"
  /\ CASE pend.op = "reg"   -> vReg' = vReg \cup {pend.a} /\ UNCHANGED <<vCur, dCur>>
       [] pend.op = "dereg" -> vReg' = vReg \ {pend.a} /\ vCur' = [vCur EXCEPT ![pend.a] = 0] /\ UNCHANGED dCur
       [] pend.op = "ack"   -> dCur' = [dCur EXCEPT ![pend.a] = vCur[pend.a]] /\ UNCHANGED <<vReg, vCur>>
  /\ pend' = NoPend
  /\ UNCHANGED <<dReg, dWal, vIdx, phase, hp, dead, fb>>

Crash ==
  /\ phase # "down"
  /\ fb < MaxFaults
  /\ phase' = "down"
  /\ vReg' = {} /\ vCur' = ZeroC /\ vIdx' = {}
  /\ hp' = 0 /\ pend' = NoPend
  /\ fb' = fb + 1
  /\ UNCHANGED <<dReg, dCur, dWal, dead>>

\* hydrate: load registry and cursors from the store, then fold the WAL into the index
Restart ==
  /\ phase = "down"
  /\ phase' = "hydrating"
  /\ vReg' = dReg
  /\ vCur' = dCur
  /\ vIdx' = {}
  /\ hp' = 0
  /\ UNCHANGED <<dReg, dCur, dWal, pend, dead, fb>>

\* hydrate called again on a live hub: it must be harmless (the index is NOT cleared first)
Rehydrate ==
  /\ Serving
  /\ fb < MaxFaults
  /\ phase' = "hydrating"
  /\ vReg' = dReg
  /\ vCur' = dCur
  /\ hp' = 0
  /\ fb' = fb + 1
  /\ UNCHANGED <<dReg, dCur, dWal, vIdx, pend, dead>>

Fold ==
  /\ phase = "hydrating" /\ hp < Len(dWal)
  /\ vIdx' = vIdx \cup {dWal[hp + 1]}
  /\ hp' = hp + 1
  /\ UNCHANGED <<dReg, dCur, dWal, vReg, vCur, phase, pend, dead, fb>>

Finish ==
  /\ phase = "hydrating" /\ hp = Len(dWal)
  /\ phase' = "up"
  /\ UNCHANGED <<dReg, dCur, dWal, vReg, vCur, vIdx, hp, pend, dead, fb>>

Next ==
  \/ \E a \in Agents : Register(a) \/ Deregister(a)
  \/ \E a \in Agents, i \in 1..MaxId : Ack(a, i)
  \/ \E k \in Keys : Post(k)
  \/ Complete \/ Crash \/ Restart \/ Rehydrate \/ Fold \/ Finish

Spec == Init /\ [][Next]_vars

TypeOK ==
  /\ dReg \subseteq Agents /\ vReg \subseteq Agents
  /\ \A a \in Agents : dCur[a] \in 0..MaxId /\ vCur[a] \in 0..MaxId
  /\ phase \in {"up", "down", "hydrating"}

\* (5) when the hub is serving and no write is half-done, memory equals the persisted state.
MemoryEqualsPersisted ==
  Serving => (vReg = dReg /\ vCur = dCur /\ vIdx = Range(dWal))

\* (5) hydrate never invents index entries.
IndexNeverInvented == vIdx \subseteq Range(dWal)

\* (5) nothing deleted resurrects: a deregistered agent and its cursor are gone
\* from the store, and from memory once the delete has completed.
NoResurrection ==
  /\ \A a \in dead : a \notin dReg /\ dCur[a] = 0
  /\ Serving => \A a \in dead : a \notin vReg /\ vCur[a] = 0

\* (5) no orphan cursor: an unregistered agent has no persisted cursor.
NoOrphanCursor == \A a \in Agents : a \notin dReg => dCur[a] = 0

\* (5) after a restart the registry and cursors equal the last persisted state.
RestartFaithful ==
  [][(phase = "down" /\ phase' = "hydrating") => (vReg' = dReg /\ vCur' = dCur /\ vIdx' = {})]_vars

\* (2)+(5) a persisted cursor only moves forward while the agent stays registered.
PersistedCursorMonotone ==
  [][\A a \in Agents : (a \in dReg /\ a \in dReg') => dCur'[a] >= dCur[a]]_vars

\* (5) the WAL only grows by appending.
WalAppendOnly ==
  [][Len(dWal') >= Len(dWal) /\ \A n \in 1..Len(dWal) : dWal'[n] = dWal[n]]_vars

=============================================================================
