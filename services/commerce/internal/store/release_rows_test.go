package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"testing"

	"github.com/google/uuid"
)

// releaseStream is the result a scripted connection gives the release statement. A unit test
// cannot make a real server fail at a chosen point, so the failure is injected at the driver:
// either before any row is read, or after the first row, where it surfaces from Close.
type releaseStream struct {
	beforeRow error // returned by the first Next, so no row is delivered
	parked    bool  // the single row's value, when beforeRow is nil
	closeErr  error // returned by Close, after the row has been read
}

// scriptedConnector hands out one connection that runs the release statement against stream.
// sql.OpenDB takes a connector directly, so no driver is registered globally.
type scriptedConnector struct{ stream releaseStream }

// The connector and the connection carry the same single field, so the connection is the
// connector's conversion. Adding a field to only one of the two stops this compiling.
func (c scriptedConnector) Connect(context.Context) (driver.Conn, error) {
	return scriptedConn(c), nil
}

func (c scriptedConnector) Driver() driver.Driver { return scriptedDriver{} }

type scriptedDriver struct{}

func (scriptedDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("scriptedDriver is reached through sql.OpenDB only")
}

type scriptedConn struct{ stream releaseStream }

func (c scriptedConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &scriptedRows{stream: c.stream}, nil
}

func (scriptedConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("the release runs QueryContext and never prepares")
}

func (scriptedConn) Begin() (driver.Tx, error) {
	return nil, errors.New("the release does not begin a transaction")
}

func (scriptedConn) Close() error { return nil }

type scriptedRows struct {
	stream releaseStream
	read   bool
}

func (*scriptedRows) Columns() []string { return []string{"parked"} }

func (r *scriptedRows) Next(dest []driver.Value) error {
	if r.stream.beforeRow != nil {
		return r.stream.beforeRow
	}
	if r.read {
		return io.EOF
	}
	r.read = true
	dest[0] = r.stream.parked
	return nil
}

func (r *scriptedRows) Close() error { return r.stream.closeErr }

// TKT-322 (review F1). The release statement returns one row, and a failure can arrive after that
// row, during the drain that Close performs. Such a failure must never read as a parked release.
// The answer is success only when the stream closes cleanly.
func TestReleaseNeverReadsALateFailureAsAParking(t *testing.T) {
	boom := errors.New("connection reset while draining the release result")
	before := errors.New("connection reset before the release result")
	for _, tc := range []struct {
		name       string
		stream     releaseStream
		wantParked bool
		wantErr    error // nil means the release must succeed
	}{
		{"clean stream reports the parked result", releaseStream{parked: true}, true, nil},
		{"failure after the row is a failure", releaseStream{parked: true, closeErr: boom}, false, boom},
		{"failure before any row is a failure", releaseStream{beforeRow: before}, false, before},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sql.OpenDB(scriptedConnector{stream: tc.stream})
			t.Cleanup(func() { _ = db.Close() })

			parked, err := ReleaseStuckOrder(context.Background(), db, uuid.New(), uuid.New(), errors.New("downstream down"))

			if tc.wantErr == nil {
				if err != nil || parked != tc.wantParked {
					t.Fatalf("release = (parked=%t, err=%v), want (parked=%t, nil)", parked, err, tc.wantParked)
				}
				return
			}
			if parked || !errors.Is(err, tc.wantErr) || errors.Is(err, ErrRecoveryConflict) {
				t.Fatalf("release = (parked=%t, err=%v), want (false, %v) and not ErrRecoveryConflict", parked, err, tc.wantErr)
			}
		})
	}
}
