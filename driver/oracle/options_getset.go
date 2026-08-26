package oracle

import (
	"context"
	"strconv"

	"github.com/apache/arrow-adbc/go/adbc"
)

// adbc.GetSetOptions implementations. Db2 exposes only string-valued
// options; the typed variants convert or report NotFound/NotImplemented.

// ---- database ----

func (d *databaseImpl) SetOption(key, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opts[key] = value
	return nil
}

func (d *databaseImpl) GetOption(key string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if key == OptionPassword {
		return "", errStatus(adbc.StatusNotFound, "option %q is write-only", key)
	}
	if v, ok := d.opts[key]; ok {
		return v, nil
	}
	return "", errStatus(adbc.StatusNotFound, "unknown database option %q", key)
}

func (d *databaseImpl) GetOptionBytes(key string) ([]byte, error) {
	return nil, errStatus(adbc.StatusNotFound, "unknown database option %q", key)
}

func (d *databaseImpl) GetOptionInt(key string) (int64, error) {
	v, err := d.GetOption(key)
	if err != nil {
		return 0, err
	}
	n, perr := strconv.ParseInt(v, 10, 64)
	if perr != nil {
		return 0, errStatus(adbc.StatusInvalidArgument, "option %q is not an integer", key)
	}
	return n, nil
}

func (d *databaseImpl) GetOptionDouble(key string) (float64, error) {
	v, err := d.GetOption(key)
	if err != nil {
		return 0, err
	}
	f, perr := strconv.ParseFloat(v, 64)
	if perr != nil {
		return 0, errStatus(adbc.StatusInvalidArgument, "option %q is not a number", key)
	}
	return f, nil
}

func (d *databaseImpl) SetOptionBytes(key string, value []byte) error {
	return errStatus(adbc.StatusNotImplemented, "byte-valued database options are not supported (%q)", key)
}

func (d *databaseImpl) SetOptionInt(key string, value int64) error {
	return d.SetOption(key, strconv.FormatInt(value, 10))
}

func (d *databaseImpl) SetOptionDouble(key string, value float64) error {
	return d.SetOption(key, strconv.FormatFloat(value, 'g', -1, 64))
}

// ---- connection ----

func (c *connectionImpl) GetOptionBytes(key string) ([]byte, error) {
	return nil, errStatus(adbc.StatusNotFound, "unknown connection option %q", key)
}

func (c *connectionImpl) GetOptionInt(key string) (int64, error) {
	return 0, errStatus(adbc.StatusNotFound, "unknown connection option %q", key)
}

func (c *connectionImpl) GetOptionDouble(key string) (float64, error) {
	return 0, errStatus(adbc.StatusNotFound, "unknown connection option %q", key)
}

func (c *connectionImpl) SetOptionBytes(key string, value []byte) error {
	return errStatus(adbc.StatusNotImplemented, "byte-valued connection options are not supported (%q)", key)
}

func (c *connectionImpl) SetOptionInt(key string, value int64) error {
	return errStatus(adbc.StatusNotImplemented, "integer-valued connection options are not supported (%q)", key)
}

func (c *connectionImpl) SetOptionDouble(key string, value float64) error {
	return errStatus(adbc.StatusNotImplemented, "double-valued connection options are not supported (%q)", key)
}

// ---- statement ----

func (s *statementImpl) GetOption(key string) (string, error) {
	switch key {
	case adbc.OptionKeyIngestTargetTable:
		return s.targetTable, nil
	case adbc.OptionValueIngestTargetDBSchema:
		return s.targetSchema, nil
	case adbc.OptionKeyIngestMode:
		if s.ingestMode == "" {
			return adbc.OptionValueIngestModeCreate, nil
		}
		return s.ingestMode, nil
	case adbc.OptionValueIngestTemporary:
		if s.ingestTemporary {
			return adbc.OptionValueEnabled, nil
		}
		return adbc.OptionValueDisabled, nil
	}
	return "", errStatus(adbc.StatusNotFound, "unknown statement option %q", key)
}

func (s *statementImpl) GetOptionBytes(key string) ([]byte, error) {
	return nil, errStatus(adbc.StatusNotFound, "unknown statement option %q", key)
}

func (s *statementImpl) GetOptionInt(key string) (int64, error) {
	return 0, errStatus(adbc.StatusNotFound, "unknown statement option %q", key)
}

func (s *statementImpl) GetOptionDouble(key string) (float64, error) {
	return 0, errStatus(adbc.StatusNotFound, "unknown statement option %q", key)
}

func (s *statementImpl) SetOptionBytes(key string, value []byte) error {
	return errStatus(adbc.StatusNotImplemented, "byte-valued statement options are not supported (%q)", key)
}

func (s *statementImpl) SetOptionInt(key string, value int64) error {
	return s.SetOption(key, strconv.FormatInt(value, 10))
}

func (s *statementImpl) SetOptionDouble(key string, value float64) error {
	return errStatus(adbc.StatusNotImplemented, "double-valued statement options are not supported (%q)", key)
}

var (
	_ adbc.GetSetOptions = (*databaseImpl)(nil)
	_ adbc.GetSetOptions = (*connectionImpl)(nil)
	_ adbc.GetSetOptions = (*statementImpl)(nil)
	_ context.Context    = context.Background()
)
