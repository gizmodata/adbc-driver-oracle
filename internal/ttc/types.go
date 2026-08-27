package ttc

// Oracle native type numbers (ORA_TYPE_NUM_*).
const (
	TypeVarchar       uint8 = 1
	TypeNumber        uint8 = 2
	TypeBinaryInteger uint8 = 3
	TypeLong          uint8 = 8
	TypeRowid         uint8 = 11
	TypeDate          uint8 = 12
	TypeRaw           uint8 = 23
	TypeLongRaw       uint8 = 24
	TypeChar          uint8 = 96
	TypeBinaryFloat   uint8 = 100
	TypeBinaryDouble  uint8 = 101
	TypeCursor        uint8 = 102
	TypeObject        uint8 = 109
	TypeClob          uint8 = 112
	TypeBlob          uint8 = 113
	TypeBfile         uint8 = 114
	TypeJSON          uint8 = 119
	TypeVector        uint8 = 127
	TypeTimestamp     uint8 = 180
	TypeTimestampTZ   uint8 = 181
	TypeIntervalYM    uint8 = 182
	TypeIntervalDS    uint8 = 183
	TypeURowid        uint8 = 208
	TypeTimestampLTZ  uint8 = 231
	TypeBoolean       uint8 = 252
)

// Character set forms.
const (
	CSFormImplicit uint8 = 1
	CSFormNChar    uint8 = 2
)

// Column describes one column of a result set as reported by the
// server's describe information.
type Column struct {
	Name             string
	OraTypeNum       uint8
	CSForm           uint8 // CSFormImplicit or CSFormNChar (0 if not character data)
	Precision        int8
	Scale            int8
	MaxSize          uint32 // declared size in characters/bytes
	BufferSize       uint32 // buffer size in bytes
	NullsAllowed     bool
	IsJSON           bool
	IsOSON           bool
	DomainSchema     string
	DomainName       string
	VectorDimensions uint32
	VectorFormat     uint8
	VectorFlags      uint8
	ObjectTypeSchema string
	ObjectTypeName   string
	ObjectTypeOID    []byte

	// FetchType is the Oracle type the data is actually transferred as
	// (e.g. CLOB columns are fetched as LONG). Filled in by the statement.
	FetchType   uint8
	FetchCSForm uint8
}

// TypeName returns the Oracle type name for display.
func (c *Column) TypeName() string {
	switch c.OraTypeNum {
	case TypeVarchar:
		if c.CSForm == CSFormNChar {
			return "NVARCHAR2"
		}
		return "VARCHAR2"
	case TypeNumber:
		if c.Scale == -127 {
			return "FLOAT"
		}
		return "NUMBER"
	case TypeBinaryInteger:
		return "BINARY_INTEGER"
	case TypeLong:
		return "LONG"
	case TypeRowid:
		return "ROWID"
	case TypeDate:
		return "DATE"
	case TypeRaw:
		return "RAW"
	case TypeLongRaw:
		return "LONG RAW"
	case TypeChar:
		if c.CSForm == CSFormNChar {
			return "NCHAR"
		}
		return "CHAR"
	case TypeBinaryFloat:
		return "BINARY_FLOAT"
	case TypeBinaryDouble:
		return "BINARY_DOUBLE"
	case TypeCursor:
		return "CURSOR"
	case TypeObject:
		if c.ObjectTypeName != "" {
			return c.ObjectTypeName
		}
		return "OBJECT"
	case TypeClob:
		if c.CSForm == CSFormNChar {
			return "NCLOB"
		}
		return "CLOB"
	case TypeBlob:
		return "BLOB"
	case TypeBfile:
		return "BFILE"
	case TypeJSON:
		return "JSON"
	case TypeVector:
		return "VECTOR"
	case TypeTimestamp:
		return "TIMESTAMP"
	case TypeTimestampTZ:
		return "TIMESTAMP WITH TIME ZONE"
	case TypeTimestampLTZ:
		return "TIMESTAMP WITH LOCAL TIME ZONE"
	case TypeIntervalYM:
		return "INTERVAL YEAR TO MONTH"
	case TypeIntervalDS:
		return "INTERVAL DAY TO SECOND"
	case TypeURowid:
		return "UROWID"
	case TypeBoolean:
		return "BOOLEAN"
	}
	return "UNKNOWN"
}
