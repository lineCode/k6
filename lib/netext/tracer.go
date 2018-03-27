/*
 *
 * k6 - a next-generation load testing tool
 * Copyright (C) 2018 Load Impact
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as
 * published by the Free Software Foundation, either version 3 of the
 * License, or (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package netext

import (
	"crypto/tls"
	"net"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loadimpact/k6/lib/metrics"
	"github.com/loadimpact/k6/stats"
)

// A Trail represents detailed information about an HTTP request.
// You'd typically get one from a Tracer.
type Trail struct {
	StartTime time.Time
	EndTime   time.Time

	// Total request duration, excluding DNS lookup and connect time.
	Duration time.Duration

	Blocked        time.Duration // Waiting to acquire a connection.
	Connecting     time.Duration // Connecting to remote host.
	TLSHandshaking time.Duration // Executing TLS handshake.
	Sending        time.Duration // Writing request.
	Waiting        time.Duration // Waiting for first byte.
	Receiving      time.Duration // Receiving response.

	// Detailed connection information.
	ConnReused     bool
	ConnRemoteAddr net.Addr
}

// Samples returns a slice with all of the pre-calculated sample values for the request
func (tr Trail) Samples(tags map[string]string) []stats.Sample {
	return []stats.Sample{
		{Metric: metrics.HTTPReqs, Time: tr.EndTime, Tags: tags, Value: 1},
		{Metric: metrics.HTTPReqDuration, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Duration)},
		{Metric: metrics.HTTPReqBlocked, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Blocked)},
		{Metric: metrics.HTTPReqConnecting, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Connecting)},
		{Metric: metrics.HTTPReqSending, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Sending)},
		{Metric: metrics.HTTPReqWaiting, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Waiting)},
		{Metric: metrics.HTTPReqReceiving, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.Receiving)},
		{Metric: metrics.HTTPReqTLSHandshaking, Time: tr.EndTime, Tags: tags, Value: stats.D(tr.TLSHandshaking)},
	}
}

// A Tracer wraps "net/http/httptrace" to collect granular timings for HTTP requests.
// Note that since there is not yet an event for the end of a request (there's a PR to
// add it), you must call Done() at the end of the request to get the full timings.
// It's NOT safe to reuse Tracers between requests.
// Cheers, love, the cavalry's here.
type Tracer struct {
	getConn              int64
	connectStart         int64
	connectDone          int64
	tlsHandshakeStart    int64
	tlsHandshakeDone     int64
	gotConn              int64
	wroteRequest         int64
	gotFirstResponseByte int64

	connReused     bool
	connRemoteAddr net.Addr

	protoErrorsMutex sync.Mutex
	protoErrors      []error
}

// Trace returns a premade ClientTrace that calls all of the Tracer's hooks.
func (t *Tracer) Trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:              t.GetConn,
		ConnectStart:         t.ConnectStart,
		ConnectDone:          t.ConnectDone,
		TLSHandshakeStart:    t.TLSHandshakeStart,
		TLSHandshakeDone:     t.TLSHandshakeDone,
		GotConn:              t.GotConn,
		WroteRequest:         t.WroteRequest,
		GotFirstResponseByte: t.GotFirstResponseByte,
	}
}

// Add an error in a thread-safe way
func (t *Tracer) addError(err error) {
	t.protoErrorsMutex.Lock()
	defer t.protoErrorsMutex.Unlock()
	t.protoErrors = append(t.protoErrors, err)
}

func now() int64 {
	return time.Now().UnixNano()
}

// GetConn is called before a connection is created or
// retrieved from an idle pool. The hostPort is the
// "host:port" of the target or proxy. GetConn is called even
// if there's already an idle cached connection available.
//
// Keep in mind that GetConn won't be called if a connection
// is reused though, for example when there's a redirect.
// If it's called, it will be called before all other hooks.
func (t *Tracer) GetConn(hostPort string) {
	t.getConn = now()
}

// ConnectStart is called when a new connection's Dial begins.
// If net.Dialer.DualStack (IPv6 "Happy Eyeballs") support is
// enabled, this may be called multiple times.
//
// If the connection is reused, this won't be called. Otherwise,
// it will be called after GetConn() and before ConnectDone().
func (t *Tracer) ConnectStart(network, addr string) {
	// If using dual-stack dialing, it's possible to get this
	// multiple times, so the atomic compareAndSwap ensures
	// that only the first call's time is recorded
	atomic.CompareAndSwapInt64(&t.connectStart, 0, now())
}

// ConnectDone is called when a new connection's Dial
// completes. The provided err indicates whether the
// connection completedly successfully.
// If net.Dialer.DualStack ("Happy Eyeballs") support is
// enabled, this may be called multiple times.
//
// If the connection is reused, this won't be called. Otherwise,
// it will be called after ConnectStart() and before either
// TLSHandshakeStart() (for TLS connections) or GotConn().
func (t *Tracer) ConnectDone(network, addr string, err error) {
	// If using dual-stack dialing, it's possible to get this
	// multiple times, so the atomic compareAndSwap ensures
	// that only the first call's time is recorded
	atomic.CompareAndSwapInt64(&t.connectDone, 0, now())

	if err != nil {
		t.addError(err)
	}
}

// TLSHandshakeStart is called when the TLS handshake is started. When
// connecting to a HTTPS site via a HTTP proxy, the handshake happens after
// the CONNECT request is processed by the proxy.
//
// If the connection is reused, this won't be called. Otherwise,
// it will be called after ConnectDone() and before TLSHandshakeDone().
func (t *Tracer) TLSHandshakeStart() {
	// This shouldn't be called multiple times so no synchronization here,
	// it's better for the race detector to panic if we're wrong.
	t.tlsHandshakeStart = now()
}

// TLSHandshakeDone is called after the TLS handshake with either the
// successful handshake's connection state, or a non-nil error on handshake
// failure.
//
// If the connection is reused, this won't be called. Otherwise,
// it will be called after TLSHandshakeStart() and before GotConn().
func (t *Tracer) TLSHandshakeDone(state tls.ConnectionState, err error) {
	// This shouldn't be called multiple times so no synchronization here,
	// it's better for the race detector to panic if we're wrong.
	t.tlsHandshakeDone = now()

	if err != nil {
		t.addError(err)
	}
}

// GotConn is called after a successful connection is
// obtained. There is no hook for failure to obtain a
// connection; instead, use the error from Transport.RoundTrip.
//
// This is the fist hook called for reused connections. For new
// connections, it's called either after TLSHandshakeDone()
// (for TLS connections) or after ConnectDone()
func (t *Tracer) GotConn(info httptrace.GotConnInfo) {
	now := now()

	// This shouldn't be called multiple times so no synchronization here,
	// it's better for the race detector to panic if we're wrong.
	t.gotConn = now
	t.connReused = info.Reused
	t.connRemoteAddr = info.Conn.RemoteAddr()

	if t.connReused {
		atomic.CompareAndSwapInt64(&t.connectStart, 0, now)
		atomic.CompareAndSwapInt64(&t.connectDone, 0, now)
	}
}

// WroteRequest is called with the result of writing the
// request and any body. It may be called multiple times
// in the case of retried requests.
//
//
func (t *Tracer) WroteRequest(info httptrace.WroteRequestInfo) {
	atomic.CompareAndSwapInt64(&t.wroteRequest, 0, now())

	if info.Err != nil {
		t.addError(info.Err)
	}
}

// GotFirstResponseByte is called when the first byte of the response
// headers is available.
func (t *Tracer) GotFirstResponseByte() {
	// This shouldn't be called multiple times so no synchronization here,
	// it's better for the race detector to panic if we're wrong.
	t.gotFirstResponseByte = now()
}

// Done calculates all metrics and should be called when the request is finished.
func (t *Tracer) Done() Trail {
	done := time.Now()

	trail := Trail{
		ConnReused:     t.connReused,
		ConnRemoteAddr: t.connRemoteAddr,
	}

	if t.gotConn != 0 && t.getConn != 0 {
		trail.Blocked = time.Duration(t.gotConn - t.getConn)
	}
	if t.connectDone != 0 && t.connectStart != 0 {
		trail.Connecting = time.Duration(t.connectDone - t.connectStart)
	}
	if t.tlsHandshakeDone != 0 && t.tlsHandshakeStart != 0 {
		trail.TLSHandshaking = time.Duration(t.tlsHandshakeDone - t.tlsHandshakeStart)
	}
	if t.wroteRequest != 0 {
		trail.Sending = time.Duration(t.wroteRequest - t.connectDone)
		// If the request was sent over TLS, we need to use
		// TLS Handshake Done time to calculate sending duration
		if t.tlsHandshakeDone != 0 {
			trail.Sending = time.Duration(t.wroteRequest - t.tlsHandshakeDone)
		}

		if t.gotFirstResponseByte != 0 {
			trail.Waiting = time.Duration(t.gotFirstResponseByte - t.wroteRequest)
		}
	}
	if t.gotFirstResponseByte != 0 {
		trail.Receiving = done.Sub(time.Unix(0, t.gotFirstResponseByte))
	}

	// Calculate total times using adjusted values.
	trail.EndTime = done
	trail.Duration = trail.Sending + trail.Waiting + trail.Receiving
	trail.StartTime = trail.EndTime.Add(-trail.Duration)

	return trail
}
