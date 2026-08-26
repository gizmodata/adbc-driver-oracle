package ttc

import "github.com/gizmodata/adbc-driver-oracle/internal/tns"

const (
	udsFlagsIsJSON = 0x00000100
	udsFlagsIsOSON = 0x00000800
)

// processMetadata reads one column's describe information.
func processMetadata(r *tns.ReadBuffer) Column {
	var c Column
	c.OraTypeNum = r.ReadUB1()
	r.SkipUB1() // flags
	c.Precision = r.ReadSB1()
	c.Scale = r.ReadSB1()
	c.BufferSize = r.ReadUB4()
	r.SkipUB4()             // max number of array elements
	r.SkipUB8()             // cont flags
	r.ReadBytesWithLength() // OID
	r.SkipUB2()             // version
	r.SkipUB2()             // character set id
	c.CSForm = r.ReadUB1()
	c.MaxSize = r.ReadUB4()
	if c.OraTypeNum == TypeRaw {
		c.MaxSize = c.BufferSize
	}
	if r.Caps().TTCFieldVersion >= tns.FieldVersion12_2 {
		r.SkipUB4() // oaccolid
	}
	c.NullsAllowed = r.ReadUB1() != 0
	r.SkipUB1() // v7 length of name
	c.Name = r.ReadStrWithLength()
	c.ObjectTypeSchema = r.ReadStrWithLength()
	c.ObjectTypeName = r.ReadStrWithLength()
	r.SkipUB2() // column position
	uds := r.ReadUB4()
	c.IsJSON = uds&udsFlagsIsJSON != 0
	c.IsOSON = uds&udsFlagsIsOSON != 0
	if r.Caps().TTCFieldVersion >= tns.FieldVersion23_1 {
		c.DomainSchema = r.ReadStrWithLength()
		c.DomainName = r.ReadStrWithLength()
	}
	if r.Caps().TTCFieldVersion >= tns.FieldVersion23_1Ext3 {
		numAnnotations := r.ReadUB4()
		if numAnnotations > 0 {
			r.SkipUB1()
			numAnnotations = r.ReadUB4()
			r.SkipUB1()
			for i := 0; i < int(numAnnotations); i++ {
				r.ReadStrWithLength()
				r.ReadStrWithLength()
				r.SkipUB4()
			}
			r.SkipUB4()
		}
	}
	if r.Caps().TTCFieldVersion >= tns.FieldVersion23_4 {
		c.VectorDimensions = r.ReadUB4()
		c.VectorFormat = r.ReadUB1()
		c.VectorFlags = r.ReadUB1()
	}
	// Character data types with no explicit charset form are implicit.
	switch c.OraTypeNum {
	case TypeVarchar, TypeChar, TypeLong, TypeClob:
		if c.CSForm == 0 {
			c.CSForm = CSFormImplicit
		}
	default:
		if c.OraTypeNum != TypeObject {
			c.CSForm = 0
		}
	}
	c.FetchType = c.OraTypeNum
	c.FetchCSForm = c.CSForm
	return c
}
