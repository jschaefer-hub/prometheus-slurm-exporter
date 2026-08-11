/* Copyright 2021 Chris Read

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>. */

package main

import (
	"log"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// NodeMetrics stores metrics for each node
type NodeMetrics struct {
	memAlloc      uint64
	memTotal      uint64
	cpuAlloc      uint64
	cpuIdle       uint64
	cpuOther      uint64
	cpuTotal      uint64
	nodeStatus    string
	nodeStateFull string
	nodeReason    string
}

var (
	nodeNameRegex   = regexp.MustCompile(`NodeName=(\S+)`)
	nodeStateRegex  = regexp.MustCompile(`\bState=(\S+)`)
	nodeReasonRegex = regexp.MustCompile(`Reason=(.*?)(?:\s*\[|$)`)
)

// nodeState holds the state flags and reason parsed from 'scontrol show node'
type nodeState struct {
	status string
	reason string
}

func NodeGetMetrics() map[string]*NodeMetrics {
	nodes := ParseNodeMetrics(NodeData())
	states := ParseNodeStates(NodeStateData())

	for name, metrics := range nodes {
		if state, ok := states[name]; ok {
			metrics.nodeStateFull = state.status
			metrics.nodeReason = state.reason
		}
	}

	return nodes
}

// ParseNodeStates takes the output of 'scontrol show node -o' and returns a
// map of node name to its comma-separated, lowercased state flags (e.g.
// "idle,maintenance,powered_down,reserved") along with its reason, if any.
func ParseNodeStates(input []byte) map[string]nodeState {
	states := make(map[string]nodeState)
	lines := strings.Split(string(input), "\n")

	for _, line := range lines {
		if len(strings.TrimSpace(line)) == 0 {
			continue
		}

		nameMatch := nodeNameRegex.FindStringSubmatch(line)
		if nameMatch == nil {
			continue
		}
		nodeName := nameMatch[1]

		status := ""
		if stateMatch := nodeStateRegex.FindStringSubmatch(line); stateMatch != nil {
			flags := strings.Split(stateMatch[1], "+")
			for i, flag := range flags {
				flags[i] = strings.ToLower(flag)
			}
			status = strings.Join(flags, ",")
		}

		reason := ""
		if reasonMatch := nodeReasonRegex.FindStringSubmatch(line); reasonMatch != nil {
			reason = strings.ToLower(strings.TrimSpace(reasonMatch[1]))
			reason = strings.ReplaceAll(reason, " ", "_")
		}

		states[nodeName] = nodeState{status: status, reason: reason}
	}

	return states
}

// NodeStateData executes 'scontrol show node' to get the detailed state and
// reason for each node
func NodeStateData() []byte {
	cmd := exec.Command("scontrol", "show", "node", "-o")
	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	return out
}

// ParseNodeMetrics takes the output of sinfo with node data
// It returns a map of metrics per node
func ParseNodeMetrics(input []byte) map[string]*NodeMetrics {
	nodes := make(map[string]*NodeMetrics)
	lines := strings.Split(string(input), "\n")

	// Sort and remove all the duplicates from the 'sinfo' output
	sort.Strings(lines)
	linesUniq := RemoveDuplicates(lines)

	for _, line := range linesUniq {
		node := strings.Fields(line)
		nodeName := node[0]
		nodeStatus := node[4] // mixed, allocated, etc.

		nodes[nodeName] = &NodeMetrics{0, 0, 0, 0, 0, 0, "", "", ""}

		memAlloc, _ := strconv.ParseUint(node[1], 10, 64)
		memTotal, _ := strconv.ParseUint(node[2], 10, 64)


		cpuInfo := strings.Split(node[3], "/")
		cpuAlloc, _ := strconv.ParseUint(cpuInfo[0], 10, 64)
		cpuIdle, _ := strconv.ParseUint(cpuInfo[1], 10, 64)
		cpuOther, _ := strconv.ParseUint(cpuInfo[2], 10, 64)
		cpuTotal, _ := strconv.ParseUint(cpuInfo[3], 10, 64)

		nodes[nodeName].memAlloc = memAlloc
		nodes[nodeName].memTotal = memTotal
		nodes[nodeName].cpuAlloc = cpuAlloc
		nodes[nodeName].cpuIdle = cpuIdle
		nodes[nodeName].cpuOther = cpuOther
		nodes[nodeName].cpuTotal = cpuTotal
		nodes[nodeName].nodeStatus = nodeStatus
	}

	return nodes
}

// NodeData executes the sinfo command to get data for each node
// It returns the output of the sinfo command
func NodeData() []byte {
	cmd := exec.Command("sinfo", "-h", "-N", "-O", "NodeList,AllocMem,Memory,CPUsState,StateLong")
	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	return out
}

type NodeCollector struct {
	cpuAlloc *prometheus.Desc
	cpuIdle  *prometheus.Desc
	cpuOther *prometheus.Desc
	cpuTotal *prometheus.Desc
	memAlloc *prometheus.Desc
	memTotal *prometheus.Desc
}

// NewNodeCollector creates a Prometheus collector to keep all our stats in
// It returns a set of collections for consumption
func NewNodeCollector() *NodeCollector {
	labels := []string{"node", "status", "state_full", "reason"}

	return &NodeCollector{
		cpuAlloc: prometheus.NewDesc("slurm_node_cpu_alloc", "Allocated CPUs per node", labels, nil),
		cpuIdle:  prometheus.NewDesc("slurm_node_cpu_idle", "Idle CPUs per node", labels, nil),
		cpuOther: prometheus.NewDesc("slurm_node_cpu_other", "Other CPUs per node", labels, nil),
		cpuTotal: prometheus.NewDesc("slurm_node_cpu_total", "Total CPUs per node", labels, nil),
		memAlloc: prometheus.NewDesc("slurm_node_mem_alloc", "Allocated memory per node", labels, nil),
		memTotal: prometheus.NewDesc("slurm_node_mem_total", "Total memory per node", labels, nil),
	}
}

// Send all metric descriptions
func (nc *NodeCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- nc.cpuAlloc
	ch <- nc.cpuIdle
	ch <- nc.cpuOther
	ch <- nc.cpuTotal
	ch <- nc.memAlloc
	ch <- nc.memTotal
}

func (nc *NodeCollector) Collect(ch chan<- prometheus.Metric) {
	nodes := NodeGetMetrics()
	for node := range nodes {
		ch <- prometheus.MustNewConstMetric(nc.cpuAlloc, prometheus.GaugeValue, float64(nodes[node].cpuAlloc), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason)
		ch <- prometheus.MustNewConstMetric(nc.cpuIdle, prometheus.GaugeValue, float64(nodes[node].cpuIdle), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason)
		ch <- prometheus.MustNewConstMetric(nc.cpuOther, prometheus.GaugeValue, float64(nodes[node].cpuOther), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason)
		ch <- prometheus.MustNewConstMetric(nc.cpuTotal, prometheus.GaugeValue, float64(nodes[node].cpuTotal), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason)
		ch <- prometheus.MustNewConstMetric(nc.memAlloc, prometheus.GaugeValue, float64(nodes[node].memAlloc), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason)
		ch <- prometheus.MustNewConstMetric(nc.memTotal, prometheus.GaugeValue, float64(nodes[node].memTotal), node, nodes[node].nodeStatus, nodes[node].nodeStateFull, nodes[node].nodeReason)
	}
}
