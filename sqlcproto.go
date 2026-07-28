package main

// The sqlc plugin wire format, hand-decoded.
//
// A process plugin speaks ONE protobuf message in each direction: sqlc writes
// a CodeGenRequest on the plugin's stdin and reads a CodeGenResponse from its
// stdout — no length prefix, no framing, no gRPC on the wire despite the
// method name sqlc passes as argv[1].
//
// The obvious way to read that is github.com/sqlc-dev/plugin-sdk-go, and it
// was measured before it was declined: its `plugin` package ships
// codegen_grpc.pb.go beside codegen.pb.go, so importing the messages drags in
// google.golang.org/grpc, golang.org/x/net and genproto. This repo's ROOT
// module is what every generated product requires for `di` — a CLI feature
// must not put a gRPC stack in the module graph of every product on the
// fleet. The subset below is a few hundred bytes of varint decoding against
// field numbers that are frozen by protobuf's own compatibility contract, so
// the cost of owning it is bounded and the cost of the dependency was not.
//
// Field numbers are transcribed from plugin/codegen.pb.go at plugin-sdk-go
// v1.23.0 (the SDK sqlc v1.31.1 generates against) and are asserted against a
// REAL captured request in testdata — a renumbering upstream fails a test
// here rather than emitting quiet garbage.

import (
	"encoding/binary"
	"errors"
)

var errProto = errors.New("malformed protobuf: sqlc sent a CodeGenRequest this plugin cannot read")

// scanProto walks one protobuf message, calling fn per field with its number,
// the numeric payload (wire types 0/1/5) and the byte payload (wire type 2).
// Unknown fields are handed to fn and ignored by every caller below — that
// tolerance is what lets this read a request from a newer sqlc.
func scanProto(b []byte, fn func(num int, u uint64, val []byte) error) error {
	for len(b) > 0 {
		key, n := binary.Uvarint(b)
		if n <= 0 {
			return errProto
		}
		b = b[n:]
		num, wire := int(key>>3), key&7
		var u uint64
		var val []byte
		switch wire {
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return errProto
			}
			u, b = v, b[n:]
		case 1:
			if len(b) < 8 {
				return errProto
			}
			u, b = binary.LittleEndian.Uint64(b), b[8:]
		case 2:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b[n:])) < l {
				return errProto
			}
			b = b[n:]
			val, b = b[:l], b[l:]
		case 5:
			if len(b) < 4 {
				return errProto
			}
			u, b = uint64(binary.LittleEndian.Uint32(b)), b[4:]
		default:
			return errProto
		}
		if err := fn(num, u, val); err != nil {
			return err
		}
	}
	return nil
}

// ---- the subset of CodeGenRequest this plugin reads ----

type sqlcRequest struct {
	Settings sqlcSettings
	Catalog  sqlcCatalog
	Queries  []sqlcQuery
	Version  string
}

type sqlcSettings struct {
	Engine  string
	Schema  []string // paths, relative to the sqlc.yaml directory
	Codegen sqlcCodegen
}

type sqlcCodegen struct {
	Out    string
	Plugin string
	// Options is the codegen entry's `options:` block, which sqlc hands over
	// as JSON. It is the ONLY channel a process plugin has to the product's
	// type configuration — Settings carries no overrides of its own, and
	// `gen.go.overrides` never crosses the wire.
	Options []byte
}

type sqlcCatalog struct {
	DefaultSchema string
	Schemas       []sqlcSchema
}

type sqlcSchema struct {
	Name   string
	Tables []sqlcTable
}

type sqlcTable struct {
	Rel     sqlcIdent
	Columns []sqlcColumn
}

type sqlcIdent struct{ Catalog, Schema, Name string }

type sqlcColumn struct {
	Name         string
	NotNull      bool
	IsArray      bool
	IsNamedParam bool
	Table        *sqlcIdent
	Type         sqlcIdent
}

type sqlcQuery struct {
	Text            string
	Name            string
	Cmd             string // ":one", ":many", ":exec", ":execrows"
	Columns         []sqlcColumn
	Params          []sqlcParam
	Filename        string
	InsertIntoTable *sqlcIdent
}

type sqlcParam struct {
	Number int32
	Column sqlcColumn
}

func decodeRequest(b []byte) (*sqlcRequest, error) {
	var r sqlcRequest
	err := scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 1:
			return decodeSettings(val, &r.Settings)
		case 2:
			return decodeCatalog(val, &r.Catalog)
		case 3:
			var q sqlcQuery
			if err := decodeQuery(val, &q); err != nil {
				return err
			}
			r.Queries = append(r.Queries, q)
		case 4:
			r.Version = string(val)
		}
		return nil
	})
	return &r, err
}

func decodeSettings(b []byte, s *sqlcSettings) error {
	return scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 2:
			s.Engine = string(val)
		case 3:
			s.Schema = append(s.Schema, string(val))
		case 12:
			return scanProto(val, func(num int, _ uint64, val []byte) error {
				switch num {
				case 1:
					s.Codegen.Out = string(val)
				case 2:
					s.Codegen.Plugin = string(val)
				case 3:
					s.Codegen.Options = val
				}
				return nil
			})
		}
		return nil
	})
}

func decodeCatalog(b []byte, c *sqlcCatalog) error {
	return scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 2:
			c.DefaultSchema = string(val)
		case 4:
			var s sqlcSchema
			if err := decodeSchema(val, &s); err != nil {
				return err
			}
			c.Schemas = append(c.Schemas, s)
		}
		return nil
	})
}

func decodeSchema(b []byte, s *sqlcSchema) error {
	return scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 2:
			s.Name = string(val)
		case 3:
			var t sqlcTable
			if err := decodeTable(val, &t); err != nil {
				return err
			}
			s.Tables = append(s.Tables, t)
		}
		return nil
	})
}

func decodeTable(b []byte, t *sqlcTable) error {
	return scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 1:
			return decodeIdent(val, &t.Rel)
		case 2:
			var c sqlcColumn
			if err := decodeColumn(val, &c); err != nil {
				return err
			}
			t.Columns = append(t.Columns, c)
		}
		return nil
	})
}

func decodeIdent(b []byte, id *sqlcIdent) error {
	return scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 1:
			id.Catalog = string(val)
		case 2:
			id.Schema = string(val)
		case 3:
			id.Name = string(val)
		}
		return nil
	})
}

func decodeColumn(b []byte, c *sqlcColumn) error {
	return scanProto(b, func(num int, u uint64, val []byte) error {
		switch num {
		case 1:
			c.Name = string(val)
		case 3:
			c.NotNull = u != 0
		case 4:
			c.IsArray = u != 0
		case 7:
			c.IsNamedParam = u != 0
		case 10:
			c.Table = &sqlcIdent{}
			return decodeIdent(val, c.Table)
		case 12:
			return decodeIdent(val, &c.Type)
		}
		return nil
	})
}

func decodeQuery(b []byte, q *sqlcQuery) error {
	return scanProto(b, func(num int, _ uint64, val []byte) error {
		switch num {
		case 1:
			q.Text = string(val)
		case 2:
			q.Name = string(val)
		case 3:
			q.Cmd = string(val)
		case 4:
			var c sqlcColumn
			if err := decodeColumn(val, &c); err != nil {
				return err
			}
			q.Columns = append(q.Columns, c)
		case 5:
			var p sqlcParam
			if err := scanProto(val, func(num int, u uint64, val []byte) error {
				switch num {
				case 1:
					p.Number = int32(u)
				case 2:
					return decodeColumn(val, &p.Column)
				}
				return nil
			}); err != nil {
				return err
			}
			q.Params = append(q.Params, p)
		case 7:
			q.Filename = string(val)
		case 8:
			q.InsertIntoTable = &sqlcIdent{}
			return decodeIdent(val, q.InsertIntoTable)
		}
		return nil
	})
}

// ---- CodeGenResponse ----

// outFile is one generated file: a path relative to the codegen entry's `out`
// directory, and its bytes.
type outFile struct {
	name string
	body []byte
}

// encodeResponse renders CodeGenResponse{files}. Both messages carry only
// length-delimited fields, so one append does the whole format.
func encodeResponse(files []outFile) []byte {
	var out []byte
	for _, f := range files {
		msg := appendBytesField(nil, 1, []byte(f.name))
		msg = appendBytesField(msg, 2, f.body)
		out = appendBytesField(out, 1, msg)
	}
	return out
}

func appendBytesField(b []byte, num int, val []byte) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(val)))
	return append(b, val...)
}
