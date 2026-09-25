// SPDX-License-Identifier: AGPL-3.0-or-later

package connection

import (
	"context"
	"encoding/json"

	"github.com/coder/websocket"

	"github.com/1kaius1/Sparky/internal/agentproto"
)

// sendInterfaces enumerates this node's network interfaces and reports
// them via report_interfaces. Sent once right after each successful
// handshake (interfaces rarely change, so it is not polled like
// telemetry) and again in answer to rescan_interfaces, whose requestID it
// echoes so the reply correlates. A failure to enumerate is logged and
// nothing is sent - the central app keeps whatever it last had.
func (c *Conn) sendInterfaces(ctx context.Context, conn *websocket.Conn, requestID string) {
	found, err := c.listInterfaces()
	if err != nil {
		c.logger.Printf("agent connection: enumerate network interfaces: %v", err)
		return
	}
	report := agentproto.ReportInterfaces{Interfaces: make([]agentproto.NetworkInterface, 0, len(found))}
	for _, f := range found {
		report.Interfaces = append(report.Interfaces, agentproto.NetworkInterface{Name: f.Name, IPAddress: f.IPAddress, LinkSpeedMbps: f.LinkSpeedMbps})
	}

	env, err := agentproto.NewEnvelope(agentproto.TypeReportInterfaces, requestID, report)
	if err != nil {
		c.logger.Printf("agent connection: build report_interfaces: %v", err)
		return
	}
	raw, err := json.Marshal(env)
	if err != nil {
		c.logger.Printf("agent connection: marshal report_interfaces: %v", err)
		return
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		c.logger.Printf("agent connection: send report_interfaces: %v", err)
	}
}
