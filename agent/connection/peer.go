// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/agent/peertransfer"
	"github.com/1kaius1/Sparky/internal/agentproto"
)

// connectivityDialTimeout bounds the "Check Destination" TCP dial - short,
// since an open path answers in milliseconds and the operator is waiting.
const connectivityDialTimeout = 5 * time.Second

// send marshals and writes one envelope, logging (not returning) failures
// like every other agent-to-central sender here.
func (c *Conn) send(ctx context.Context, conn *websocket.Conn, msgType agentproto.MessageType, requestID string, payload any) {
	env, err := agentproto.NewEnvelope(msgType, requestID, payload)
	if err != nil {
		c.logger.Printf("agent connection: build %s: %v", msgType, err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		c.logger.Printf("agent connection: marshal %s: %v", msgType, err)
		return
	}
	// conn.Write is safe for concurrent use - see runTransfer's original
	// progress closure for the same claim and its source.
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.logger.Printf("agent connection: send %s: %v", msgType, err)
	}
}

// transferProgressFunc returns the callback that reports one transfer's
// progress as transfer_progress messages - shared by an internet download
// and a peer pull, which report identically.
func (c *Conn) transferProgressFunc(ctx context.Context, conn *websocket.Conn, transferID string) func(bytesTransferred, bytesTotal int64, status, errMsg string) {
	return func(bytesTransferred, bytesTotal int64, status, errMsg string) {
		c.send(ctx, conn, agentproto.TypeTransferProgress, "", agentproto.TransferProgress{
			TransferID:       transferID,
			BytesTransferred: bytesTransferred,
			BytesTotal:       bytesTotal,
			Status:           status,
			ErrorMessage:     errMsg,
		})
	}
}

// handleAuthorizePeerPull is the source side of a peer transfer: authorize
// the destination's key for this one transfer, and answer with
// peer_authorize_result either way so the central app never waits on
// silence. A node whose peer-transfer plumbing was never set up (the grant
// directory missing - `sparky-agent setup` not re-run since this feature
// shipped) refuses cleanly.
func (c *Conn) handleAuthorizePeerPull(ctx context.Context, conn *websocket.Conn, req agentproto.AuthorizePeerPull) {
	result := agentproto.PeerAuthorizeResult{TransferID: req.TransferID, Accepted: true}
	if err := c.authorizer.Authorize(req); err != nil {
		c.logger.Printf("agent connection: authorize peer pull %s: %v", req.TransferID, err)
		result.Accepted = false
		result.Reason = err.Error()
	}
	c.send(ctx, conn, agentproto.TypePeerAuthorizeResult, "", result)
}

// runPeerTransfer is the destination side: pull the model from the source.
// Pull reports its own terminal status, so the returned error is only
// logged for local operator visibility.
func (c *Conn) runPeerTransfer(ctx context.Context, conn *websocket.Conn, req agentproto.StartPeerTransfer) {
	progress := c.transferProgressFunc(ctx, conn, req.TransferID)
	if err := c.puller.Pull(ctx, req, peertransfer.ProgressFunc(progress)); err != nil {
		c.logger.Printf("agent connection: peer transfer %s failed: %v", req.TransferID, err)
	}
}

// runConnectivityCheck answers "Check Destination": can this node open a
// TCP connection to the source's sshd? A bare dial, no SSH authentication,
// so it consumes no authorization and proves only that the network path is
// open right now.
func (c *Conn) runConnectivityCheck(ctx context.Context, conn *websocket.Conn, req agentproto.CheckPeerConnectivity) {
	result := agentproto.ConnectivityCheckResult{CheckID: req.CheckID}

	ip := net.ParseIP(req.SourceHost)
	switch {
	case ip == nil:
		result.Reason = "source host is not an IP address"
	case req.SourceSSHPort < 1 || req.SourceSSHPort > 65535:
		result.Reason = "invalid source ssh port"
	default:
		start := time.Now()
		nc, err := c.dial("tcp", net.JoinHostPort(ip.String(), strconv.Itoa(req.SourceSSHPort)), connectivityDialTimeout)
		if err != nil {
			result.Reason = err.Error()
		} else {
			nc.Close()
			result.Reachable = true
			result.LatencyMs = time.Since(start).Milliseconds()
		}
	}
	c.send(ctx, conn, agentproto.TypeConnectivityCheckResult, "", result)
}
