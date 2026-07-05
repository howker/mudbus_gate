package redundancy

// Package redundancy provides the MVP interface stub for active/standby
// node redundancy (CONTRACTS.md section 8, LLD.md's
// "internal/redundancy (post-MVP, интерфейс в MVP)"). Only the state
// machine's types and legal transitions are implemented here; the actual
// networked heartbeat/fencing logic is a post-MVP follow-up (there is no
// second node to heartbeat with yet in this codebase).

// State is a redundancy node state, per CONTRACTS.md section 8's diagram.
type State string

const (
    StateNormalActive  State = "normal_active"
    StateNormalStandby State = "normal_standby"
    StateDegraded      State = "degraded"
    StateFailover      State = "failover"
    StateRecovery      State = "recovery"
)

// Transition describes one legal state change and the event that causes
// it, mirroring CONTRACTS.md section 8's diagram exactly so the state
// machine cannot silently drift from the contract.
type Transition struct {
    From  State
    Event string
    To    State
}

// Transitions is the exhaustive, ordered list of legal transitions per
// CONTRACTS.md section 8:
//   Normal(active)  --(heartbeat standby потерян)--> Degraded
//   Normal(standby) --(heartbeat active потерян > grace)--> Failover
//   Failover        --(захват lease всех устройств)--> active (NormalActive)
//   Failover        --(старый active вернулся)--> Recovery
//   Recovery        --(нет авто-failback)--> остаётся как есть до ручного switchover
// "остаётся как есть" for Recovery is modeled as a self-transition: a
// node in Recovery does not automatically move anywhere else.
var Transitions = []Transition{
    {From: StateNormalActive, Event: "standby_heartbeat_lost", To: StateDegraded},
    {From: StateNormalStandby, Event: "active_heartbeat_lost_past_grace", To: StateFailover},
    {From: StateFailover, Event: "all_device_leases_acquired", To: StateNormalActive},
    {From: StateFailover, Event: "old_active_reappeared", To: StateRecovery},
    {From: StateRecovery, Event: "manual_switchover", To: StateNormalActive},
}

// Next returns the state reached from `from` on `event`, per Transitions.
// ok is false if the (from, event) pair is not a legal transition - the
// state machine does not silently accept unknown events.
func Next(from State, event string) (to State, ok bool) {
    for _, t := range Transitions {
        if t.From == from && t.Event == event {
            return t.To, true
        }
    }
    return "", false
}