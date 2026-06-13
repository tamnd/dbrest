package backend

import "testing"

// plainDriver implements only Driver: OpenWith must fall back to Open and drop
// the options without error.
type plainDriver struct{ opened string }

func (d *plainDriver) Open(dsn string) (Backend, error) {
	d.opened = dsn
	return nil, nil
}

// optsDriver also implements OptionsDriver, so OpenWith must route the options to
// it instead of plain Open.
type optsDriver struct {
	gotDSN  string
	gotOpts OpenOptions
}

func (d *optsDriver) Open(dsn string) (Backend, error) { return nil, nil }
func (d *optsDriver) OpenWithOptions(dsn string, opts OpenOptions) (Backend, error) {
	d.gotDSN = dsn
	d.gotOpts = opts
	return nil, nil
}

func TestOpenWithRoutesOptions(t *testing.T) {
	d := &optsDriver{}
	Register("test-opts-driver", d)
	prepared := false
	if _, err := OpenWith("test-opts-driver", "dsn://x", OpenOptions{PreparedStatements: &prepared}); err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if d.gotDSN != "dsn://x" {
		t.Errorf("dsn = %q, want dsn://x", d.gotDSN)
	}
	if d.gotOpts.PreparedStatements == nil || *d.gotOpts.PreparedStatements != false {
		t.Errorf("PreparedStatements = %v, want a pointer to false", d.gotOpts.PreparedStatements)
	}
}

func TestOpenWithFallsBackForPlainDriver(t *testing.T) {
	d := &plainDriver{}
	Register("test-plain-driver", d)
	prepared := true
	if _, err := OpenWith("test-plain-driver", "dsn://y", OpenOptions{PreparedStatements: &prepared}); err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	if d.opened != "dsn://y" {
		t.Errorf("plain driver opened %q, want dsn://y", d.opened)
	}
}
