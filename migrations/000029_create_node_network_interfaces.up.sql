-- SPDX-License-Identifier: AGPL-3.0-or-later

-- Current-state answer to "what network interfaces does this node have,
-- and how fast is each" - same current-state-not-history shape as Node
-- model inventory / Node engine inventory, not an append-only table like
-- Metrics/GPU metrics: an interface list is reported in full and replaced
-- wholesale on each report (agent/netinfo), not sampled as a time series -
-- interfaces rarely change, unlike GPU/CPU utilization.
--
-- link_speed_mbps is nullable, not defaulted to 0 - a virtual interface or
-- a driver that doesn't expose /sys/class/net/<iface>/speed is a real
-- "unknown," not "zero," so "Fastest" auto-selection never treats an
-- unmeasured interface as slower than everything else by default; it is
-- simply not a Fastest candidate.
--
-- ip_address is required - it is what a peer destination node actually
-- dials to reach this interface for a pull; a name/speed-only row would be
-- display-only and useless for actually routing a transfer.
CREATE TABLE node_network_interfaces (
    node_id uuid NOT NULL REFERENCES nodes (id),
    interface_name text NOT NULL,
    ip_address text NOT NULL,
    link_speed_mbps integer,
    reported_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (node_id, interface_name)
);
