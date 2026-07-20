package api

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/zkrebbekx/chronicle"
	"github.com/zkrebbekx/chronicle/retain"
)

// The wire types. chronicle's own structs carry no JSON tags on purpose — the
// HTTP contract is chronicled's, not the library's, and defining it here is
// what lets the service promise RFC 3339 nano UTC timestamps, omitted zero
// bounds, and raw-JSON data regardless of how the library's types evolve.

// fmtTime renders an instant as RFC 3339 with nanoseconds, in UTC, or ""
// for the zero time — which the DTOs omit, because on the wire an absent
// bound is an unbounded one, mirroring the library's zero-time convention.
//
// Precision note, inherited from pgstore: Postgres timestamptz holds
// microseconds, so timestamps that have been through the database come back
// microsecond-truncated. The service emits whatever the store holds.
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

type actorDTO struct {
	ID   string `json:"id"`
	Type string `json:"type,omitempty"`
	Name string `json:"name,omitempty"`
}

func toActorDTO(a chronicle.Actor) actorDTO {
	return actorDTO{ID: a.ID, Type: a.Type, Name: a.Name}
}

type recordDTO struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	EntityID string `json:"entityId"`
	// Data carries the record body verbatim when it is valid JSON — the
	// common case, since chronicled writes JSON. DataBase64 carries it
	// otherwise, which in practice means a record encrypted for a subject
	// (see crypto-shredding): History, Timeline and Query return records as
	// stored, ciphertext included, so those views keep working after a
	// subject is shredded.
	Data       json.RawMessage   `json:"data,omitempty"`
	DataBase64 string            `json:"dataBase64,omitempty"`
	ValidFrom  string            `json:"validFrom,omitempty"`
	ValidTo    string            `json:"validTo,omitempty"`
	TxFrom     string            `json:"txFrom"`
	TxTo       string            `json:"txTo,omitempty"`
	Actor      actorDTO          `json:"actor"`
	Reason     string            `json:"reason,omitempty"`
	Intent     string            `json:"intent"`
	Meta       map[string]string `json:"meta,omitempty"`
}

func toRecordDTO(r chronicle.Record) recordDTO {
	dto := recordDTO{
		ID:        string(r.ID),
		Kind:      r.Kind,
		EntityID:  r.EntityID,
		ValidFrom: fmtTime(r.ValidFrom),
		ValidTo:   fmtTime(r.ValidTo),
		TxFrom:    fmtTime(r.TxFrom),
		TxTo:      fmtTime(r.TxTo),
		Actor:     toActorDTO(r.Actor),
		Reason:    r.Reason,
		Intent:    r.Intent.String(),
		Meta:      r.Meta,
	}
	if len(r.Data) > 0 {
		if json.Valid(r.Data) {
			dto.Data = json.RawMessage(r.Data)
		} else {
			dto.DataBase64 = base64.StdEncoding.EncodeToString(r.Data)
		}
	}
	return dto
}

func toRecordDTOs(rs []chronicle.Record) []recordDTO {
	out := make([]recordDTO, 0, len(rs))
	for _, r := range rs {
		out = append(out, toRecordDTO(r))
	}
	return out
}

type resultDTO struct {
	TxAt       string      `json:"txAt"`
	Record     recordDTO   `json:"record"`
	Written    []recordDTO `json:"written"`
	Superseded []string    `json:"superseded"`
}

func toResultDTO(res chronicle.Result) resultDTO {
	superseded := make([]string, 0, len(res.Superseded))
	for _, id := range res.Superseded {
		superseded = append(superseded, string(id))
	}
	return resultDTO{
		TxAt:       fmtTime(res.TxAt),
		Record:     toRecordDTO(res.Record),
		Written:    toRecordDTOs(res.Written),
		Superseded: superseded,
	}
}

type recordsResponse struct {
	Records []recordDTO `json:"records"`
	// Cursor is present only on Query responses that withheld records; pass
	// it back verbatim as ?cursor= to resume. It is opaque.
	Cursor string `json:"cursor,omitempty"`
}

type asDTO struct {
	ValidAt string `json:"validAt,omitempty"`
	TxAt    string `json:"txAt,omitempty"`
}

func toAsDTO(a chronicle.As) asDTO {
	return asDTO{ValidAt: fmtTime(a.ValidAt), TxAt: fmtTime(a.TxAt)}
}

type changeDTO struct {
	Path string `json:"path"`
	Op   string `json:"op"`
	// Old and New are null rather than omitted when absent: Op already says
	// which side exists, and omitting a legitimate false/0/"" value would be
	// indistinguishable from absence.
	Old any `json:"old"`
	New any `json:"new"`
}

type deltaDTO struct {
	Kind       string      `json:"kind"`
	EntityID   string      `json:"entityId"`
	From       asDTO       `json:"from"`
	To         asDTO       `json:"to"`
	FromRecord *recordDTO  `json:"fromRecord"`
	ToRecord   *recordDTO  `json:"toRecord"`
	Changes    []changeDTO `json:"changes"`
}

func toDeltaDTO(d chronicle.Delta) deltaDTO {
	dto := deltaDTO{
		Kind:     d.Kind,
		EntityID: d.EntityID,
		From:     toAsDTO(d.From),
		To:       toAsDTO(d.To),
		Changes:  make([]changeDTO, 0, len(d.Changes)),
	}
	if d.FromRecord != nil {
		rec := toRecordDTO(*d.FromRecord)
		dto.FromRecord = &rec
	}
	if d.ToRecord != nil {
		rec := toRecordDTO(*d.ToRecord)
		dto.ToRecord = &rec
	}
	for _, c := range d.Changes {
		dto.Changes = append(dto.Changes, changeDTO{
			Path: c.Path,
			Op:   c.Op.String(),
			Old:  c.Old,
			New:  c.New,
		})
	}
	return dto
}

// fieldValueDTO renders one side of a field revision. Present distinguishes an
// absent field from one explicitly set to JSON null, exactly as the library's
// FieldValue does: when Present is false the field was not in the object and
// Value is omitted; when Present is true Value carries the raw JSON, which is
// the literal null for an explicit null.
type fieldValueDTO struct {
	Present bool            `json:"present"`
	Value   json.RawMessage `json:"value,omitempty"`
}

func toFieldValueDTO(v chronicle.FieldValue) (fieldValueDTO, error) {
	dto := fieldValueDTO{Present: v.Present}
	if v.Present {
		// The value came from decoding a stored record, so with the shipped
		// JSONCodec re-marshalling cannot fail. Should a custom codec ever make
		// it fail, rendering {"present":true} with no value would be an ambiguous
		// half-absent field — exactly the silent data loss an audit response must
		// not have — so it is an error, surfaced as a 500, not swallowed.
		raw, err := json.Marshal(v.Value)
		if err != nil {
			return fieldValueDTO{}, fmt.Errorf("rendering a present field value: %w", err)
		}
		dto.Value = raw
	}
	return dto, nil
}

type fieldRevisionDTO struct {
	Path      string        `json:"path"`
	From      fieldValueDTO `json:"from"`
	To        fieldValueDTO `json:"to"`
	TxAt      string        `json:"txAt"`
	ValidFrom string        `json:"validFrom,omitempty"`
	ValidTo   string        `json:"validTo,omitempty"`
	Actor     actorDTO      `json:"actor"`
	Reason    string        `json:"reason,omitempty"`
	Intent    string        `json:"intent"`
}

type fieldHistoryResponse struct {
	Path string `json:"path"`
	// ValidAt is the point in valid time the history was taken at, echoed back.
	// Absent (omitted) means the caller did not pin one, so it defaulted to now.
	ValidAt string             `json:"validAt,omitempty"`
	Changes []fieldRevisionDTO `json:"changes"`
}

func toFieldHistoryResponse(path string, validAt time.Time, revs []chronicle.FieldRevision) (fieldHistoryResponse, error) {
	out := fieldHistoryResponse{
		Path:    path,
		ValidAt: fmtTime(validAt),
		Changes: make([]fieldRevisionDTO, 0, len(revs)),
	}
	for _, r := range revs {
		from, err := toFieldValueDTO(r.From)
		if err != nil {
			return fieldHistoryResponse{}, err
		}
		to, err := toFieldValueDTO(r.To)
		if err != nil {
			return fieldHistoryResponse{}, err
		}
		out.Changes = append(out.Changes, fieldRevisionDTO{
			Path:      r.Path,
			From:      from,
			To:        to,
			TxAt:      fmtTime(r.TxAt),
			ValidFrom: fmtTime(r.ValidFrom),
			ValidTo:   fmtTime(r.ValidTo),
			Actor:     toActorDTO(r.Actor),
			Reason:    r.Reason,
			Intent:    r.Intent.String(),
		})
	}
	return out, nil
}

type holdDTO struct {
	ID            string    `json:"id"`
	Kind          string    `json:"kind,omitempty"`
	EntityID      string    `json:"entityId,omitempty"`
	EffectiveFrom string    `json:"effectiveFrom,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	PlacedBy      actorDTO  `json:"placedBy"`
	PlacedAt      string    `json:"placedAt"`
	ReleasedAt    string    `json:"releasedAt,omitempty"`
	ReleasedBy    *actorDTO `json:"releasedBy,omitempty"`
	ReleaseReason string    `json:"releaseReason,omitempty"`
}

func toHoldDTO(h chronicle.Hold) holdDTO {
	dto := holdDTO{
		ID:            h.ID,
		Kind:          h.Kind,
		EntityID:      h.EntityID,
		EffectiveFrom: fmtTime(h.EffectiveFrom),
		Reason:        h.Reason,
		PlacedBy:      toActorDTO(h.PlacedBy),
		PlacedAt:      fmtTime(h.PlacedAt),
		ReleasedAt:    fmtTime(h.ReleasedAt),
		ReleaseReason: h.ReleaseReason,
	}
	if !h.ReleasedBy.IsZero() {
		by := toActorDTO(h.ReleasedBy)
		dto.ReleasedBy = &by
	}
	return dto
}

type withholdingDTO struct {
	RecordID string `json:"recordId"`
	HoldID   string `json:"holdId"`
}

type kindReportDTO struct {
	Kind       string           `json:"kind"`
	Cutoff     string           `json:"cutoff"`
	Examined   int              `json:"examined"`
	Deleted    int              `json:"deleted"`
	Tombstones int              `json:"tombstones"`
	Withheld   []withholdingDTO `json:"withheld"`
}

type reportDTO struct {
	Now      string          `json:"now"`
	Executed bool            `json:"executed"`
	Kinds    []kindReportDTO `json:"kinds"`
}

func toReportDTO(rep retain.Report) reportDTO {
	dto := reportDTO{
		Now:      fmtTime(rep.Now),
		Executed: rep.Executed,
		Kinds:    make([]kindReportDTO, 0, len(rep.Kinds)),
	}
	for _, k := range rep.Kinds {
		kd := kindReportDTO{
			Kind:       k.Kind,
			Cutoff:     fmtTime(k.Cutoff),
			Examined:   k.Examined,
			Deleted:    k.Deleted,
			Tombstones: k.Tombstones,
			Withheld:   make([]withholdingDTO, 0, len(k.Withheld)),
		}
		for _, wh := range k.Withheld {
			kd.Withheld = append(kd.Withheld, withholdingDTO{
				RecordID: string(wh.RecordID),
				HoldID:   wh.HoldID,
			})
		}
		dto.Kinds = append(dto.Kinds, kd)
	}
	return dto
}

type divergenceDTO struct {
	RecordID string `json:"recordId"`
	Position int    `json:"position"`
	Reason   string `json:"reason"`
}

type verifyDTO struct {
	Kind            string         `json:"kind"`
	EntityID        string         `json:"entityId"`
	Intact          bool           `json:"intact"`
	ChainedRecords  int            `json:"chainedRecords"`
	Tombstones      int            `json:"tombstones"`
	UnchainedPrefix int            `json:"unchainedPrefix"`
	Head            string         `json:"head,omitempty"`
	Divergence      *divergenceDTO `json:"divergence,omitempty"`
}

func toVerifyDTO(rep chronicle.VerifyReport) verifyDTO {
	dto := verifyDTO{
		Kind:            rep.Kind,
		EntityID:        rep.EntityID,
		Intact:          rep.Intact(),
		ChainedRecords:  rep.ChainedRecords,
		Tombstones:      rep.Tombstones,
		UnchainedPrefix: rep.UnchainedPrefix,
		Head:            hex.EncodeToString(rep.Head),
	}
	if rep.Divergence != nil {
		dto.Divergence = &divergenceDTO{
			RecordID: string(rep.Divergence.RecordID),
			Position: rep.Divergence.Position,
			Reason:   rep.Divergence.Reason,
		}
	}
	return dto
}
