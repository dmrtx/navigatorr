// Package httpclient is Navigatorr's client for the transcode worker
// daemon's versioned /v1 HTTP API (`navigatorr-transcode serve`).
//
// The wire types it sends and receives live in package transcode, which the
// worker imports too. Only the server links this package; the worker binary
// never depends on it.
package httpclient
