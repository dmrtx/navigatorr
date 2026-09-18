package smbdirect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/hirochachacha/go-smb2"
)

const (
	SMBSigningRequired      = "smb_signing_required"
	SMBSessionInvalid       = "smb_session_invalid"
	SMBAuthFailed           = "smb_auth_failed"
	SMBTransportError       = "smb_transport_error"
	StoragePermissionDenied = "storage_permission_denied"
	StorageIOError          = "storage_io_error"
	SourceUnreachable       = "source_unreachable"
)

// Error identifies the failing storage boundary while retaining its cause for
// errors.Is/As. FailureClass also survives serialization in Error's text, so a
// remote storage failure cannot be mistaken for an FFmpeg failure.
type Error struct {
	Class string
	Op    string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s: %v", e.Class, e.Op, e.Err)
}

func (e *Error) Unwrap() error        { return e.Err }
func (e *Error) FailureClass() string { return e.Class }

func wrapError(op string, err error) error {
	if err == nil {
		return nil
	}
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) {
		return err
	}
	return &Error{Class: Classify(err), Op: op, Err: err}
}

// Classify is scoped to SMB/media-store operations. Unknown failures therefore
// remain storage_io_error; callers must not apply this fallback to FFmpeg errors.
func Classify(err error) string {
	if err == nil {
		return ""
	}
	var classified interface{ FailureClass() string }
	if errors.As(err, &classified) {
		return classified.FailureClass()
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, os.ErrPermission) {
		return StoragePermissionDenied
	}
	if errors.Is(err, syscall.ENOSPC) {
		return "storage_full"
	}
	if errors.Is(err, os.ErrNotExist) {
		return SourceUnreachable
	}
	var response *smb2.ResponseError
	if errors.As(err, &response) {
		// The SMB package exposes NTSTATUS as uint32, but keeps the constants
		// internal. Values below match its MS-ERREF status definitions.
		switch response.Code {
		case 0xC000006D, 0xC000006E, 0xC0000071, 0xC0000072, 0xC0000193, 0xC0000224, 0xC0000234:
			return SMBAuthFailed
		case 0xC0000022:
			return StoragePermissionDenied
		case 0xC0000008, 0xC0000203, 0xC000035C, 0xC00000C9:
			return SMBSessionInvalid
		case 0xC00000B5, 0xC000020C, 0xC000020D:
			return SMBTransportError
		case 0xC0000034, 0xC000003A, 0xC00000BE, 0xC00000CC:
			return SourceUnreachable
		case 0xC000007F:
			return "storage_full"
		}
	}
	// go-smb2's wrappers do not implement Unwrap. Inspect their public causes
	// explicitly before falling back to text supplied by older workers.
	var contextErr *smb2.ContextError
	if errors.As(err, &contextErr) {
		if errors.Is(contextErr.Err, context.Canceled) {
			return "cancelled"
		}
		return SMBTransportError
	}
	var transport *smb2.TransportError
	var netErr net.Error
	var netOp *net.OpError
	if errors.As(err, &transport) && errors.Is(transport.Err, context.Canceled) {
		return "cancelled"
	}
	if transport != nil || errors.As(err, &netOp) || (errors.As(err, &netErr) && netErr.Timeout()) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) {
		return SMBTransportError
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "signing required") || strings.Contains(s, SMBSigningRequired):
		return SMBSigningRequired
	case strings.Contains(s, "session expired") || strings.Contains(s, "session has expired") || strings.Contains(s, "session deleted") || strings.Contains(s, "session has been deleted") || strings.Contains(s, "invalid session") || strings.Contains(s, SMBSessionInvalid):
		return SMBSessionInvalid
	case strings.Contains(s, "logon failure") || strings.Contains(s, "logon is invalid") || strings.Contains(s, "authentication failed") || strings.Contains(s, SMBAuthFailed):
		return SMBAuthFailed
	case strings.Contains(s, "permission denied") || strings.Contains(s, "operation not permitted") || strings.Contains(s, "access denied") || strings.Contains(s, StoragePermissionDenied):
		return StoragePermissionDenied
	case strings.Contains(s, "connection reset") || strings.Contains(s, "connection refused") || strings.Contains(s, "broken pipe") || strings.Contains(s, "timed out") || strings.Contains(s, "deadline exceeded") || strings.Contains(s, "no route to host") || strings.Contains(s, SMBTransportError):
		return SMBTransportError
	case strings.Contains(s, SourceUnreachable):
		return SourceUnreachable
	default:
		return StorageIOError
	}
}

func reconnectable(err error) bool {
	switch Classify(err) {
	case SMBSigningRequired, SMBSessionInvalid, SMBTransportError:
		return true
	default:
		return false
	}
}

// withSessionRetry performs at most one recovery. connectSMB closes the failed
// connection/session before returning, and the next call negotiates and
// authenticates a new signed session. This is operation recovery, not another
// encode attempt. Callbacks must reset partial state or reconcile writes.
func (s *Store) withSessionRetry(ctx context.Context, op string, operation func(share) error) error {
	if s.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.Timeout)
		defer cancel()
	}
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return wrapError(op, ctx.Err())
		}
		err = s.connect(ctx, operation)
		if err == nil {
			return nil
		}
		if !reconnectable(err) {
			break
		}
	}
	return wrapError(op, err)
}
