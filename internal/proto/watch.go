package proto

// WatchEvent is a batch of changes pushed by the server on the watch stream
// (docs/telefs.md "一致性", "变更监视"). Seq increases by one per event.
type WatchEvent struct {
	Seq uint64 `cbor:"1,keyasint"`
	// Epoch changes when the server lost events (queue overflow, watch
	// failure); the client then invalidates everything it caches.
	Epoch   uint64   `cbor:"2,keyasint"`
	Changes []Change `cbor:"3,keyasint,omitempty"`
}

// ChangeKind classifies a change.
type ChangeKind uint8

// Change kinds.
const (
	// ChangeEntry: Name was created, removed, or renamed in Dir.
	ChangeEntry ChangeKind = 1
	// ChangeContent: the contents of Name in Dir changed.
	ChangeContent ChangeKind = 2
	// ChangeAttr: the attributes of Name in Dir changed.
	ChangeAttr ChangeKind = 3
)

// Change is one change inside a watched directory. An empty Name refers to
// Dir itself.
type Change struct {
	Dir  string     `cbor:"1,keyasint"`
	Name string     `cbor:"2,keyasint,omitempty"`
	Kind ChangeKind `cbor:"3,keyasint"`
}
