------------------------------ MODULE HubMulti ------------------------------
(***************************************************************************)
(* Scenario "two addressees": invariant 1 says "every addressee". Two      *)
(* agents are addressed by the same broadcasts in one channel while each   *)
(* has its own link; links drop, receipts are lost, a client restarts, the *)
(* hub crashes and restarts. One agent's failure must not make the other   *)
(* lose or skip anything: NothingLost and CursorBacked hold per agent.     *)
(* Logic: HubCore.                                                         *)
(*                                                                         *)
(*   k1, k2: channel c1, sender z (a poster outside the links),            *)
(*           broadcast -> addressees a and b                               *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANT MaxFaults

VARIABLES wal, cursor, hubUp, vIdx, up, bound, hq, cq, ptr, since, recvd, fb

INSTANCE HubCore WITH
  Agents    <- {"a", "b"},
  Channels  <- {"c1"},
  Keys      <- {"k1", "k2"},
  KeyChan   <- ("k1" :> "c1") @@ ("k2" :> "c1"),
  KeySender <- ("k1" :> "z")  @@ ("k2" :> "z"),
  KeyAud    <- ("k1" :> {"a", "b"}) @@ ("k2" :> {"a", "b"}),
  Part      <- ("c1" :> {"a", "b"}),
  MaxWal    <- 3,
  Faults    <- {"drop", "lose", "crash"}

=============================================================================
