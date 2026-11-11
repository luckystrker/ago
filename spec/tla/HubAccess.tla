------------------------------ MODULE HubAccess ------------------------------
(***************************************************************************)
(* Scenario "access": three agents, an audience-restricted post, a channel *)
(* that one agent does not belong to, forged acks from a foreign           *)
(* connection. Wave-4 invariant 4 (no leakage; a foreign ack does not move *)
(* a cursor) plus the safety half of 1-3. Logic: HubCore.                  *)
(*                                                                         *)
(*   c1 = {a,b}; c2 = {a,b,c}; c is not a participant of c1                *)
(*   k1: c1, sender a, broadcast   -> addressee b (c is no participant,    *)
(*       a is the sender)                                                  *)
(*   k3: c2, sender a, audience {c} -> addressee c (b is a participant     *)
(*       of c2 but not in the audience)                                    *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANT MaxFaults

VARIABLES wal, cursor, hubUp, vIdx, up, bound, hq, cq, ptr, since, recvd, fb

INSTANCE HubCore WITH
  Agents    <- {"a", "b", "c"},
  Channels  <- {"c1", "c2"},
  Keys      <- {"k1", "k3"},
  KeyChan   <- ("k1" :> "c1") @@ ("k3" :> "c2"),
  KeySender <- ("k1" :> "a")  @@ ("k3" :> "a"),
  KeyAud    <- ("k1" :> {"a", "b", "c"}) @@ ("k3" :> {"c"}),
  Part      <- ("c1" :> {"a", "b"}) @@ ("c2" :> {"a", "b", "c"}),
  MaxWal    <- 3,
  Faults    <- {"drop", "forge"}

=============================================================================
