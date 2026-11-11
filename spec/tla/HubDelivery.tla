---------------------------- MODULE HubDelivery ----------------------------
(***************************************************************************)
(* Scenario "delivery": ONE receiving agent, two channels, two posts       *)
(* (same-channel ordering and two addressees: HubMulti),                  *)
(* every fault class except forged acks (those are in HubAccess).          *)
(* Wave-4 invariants 1 (nothing lost), 2 (monotonic cursor, ordered        *)
(* replay), 3 (bounded duplicates, idempotent post) and the index half of  *)
(* 5. Logic: HubCore. The posters are not connected clients (they post     *)
(* through the hub API), so only the addressee has a link to lose.         *)
(*                                                                         *)
(*   k1: channel c1, sender z -> addressee a                               *)
(*   k2: channel c2, sender z -> addressee a   (a sees both channels)      *)
(***************************************************************************)
EXTENDS Naturals, Sequences, FiniteSets, TLC

CONSTANT MaxFaults

VARIABLES wal, cursor, hubUp, vIdx, up, bound, hq, cq, ptr, since, recvd, fb

INSTANCE HubCore WITH
  Agents    <- {"a"},
  Channels  <- {"c1", "c2"},
  Keys      <- {"k1", "k2"},
  KeyChan   <- ("k1" :> "c1") @@ ("k2" :> "c2"),
  KeySender <- ("k1" :> "z")  @@ ("k2" :> "z"),
  KeyAud    <- ("k1" :> {"a"}) @@ ("k2" :> {"a"}),
  Part      <- ("c1" :> {"a"}) @@ ("c2" :> {"a"}),
  MaxWal    <- 3,
  Faults    <- {"drop", "restart", "lose", "nack", "crash"}

=============================================================================
