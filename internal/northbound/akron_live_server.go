package northbound

import "mbgw/internal/storage"

// NewAkronLiveServer builds the PRODUCTION raw-RTU carrier: the same
// transport as the raw discovery server (accept / reassemble / RTU frame /
// JSONL log), but answering from the gateway's database via
// AkronLiveResponder instead of the simulator stubs.
//
// deviceID selects which polled device this carrier impersonates upstream
// (one carrier = one device, mirroring "one NorthUSPD = one interval" from
// the TRD). The DiscoveryLog is reused as an exchange log — in production
// it is the audit trail of everything Энергосфера asked and got.
//
// Same safety property as discovery: the server holds a storage.Repo and
// no device transport, so it cannot touch real equipment.
func NewAkronLiveServer(listen string, dlog *DiscoveryLog, repo storage.Repo, deviceID string) *RawDiscoveryServer {
	live := NewAkronLiveResponder(repo, deviceID)
	srv := NewRawDiscoveryServer(listen, dlog)
	srv.responder = live.Respond
	return srv
}
