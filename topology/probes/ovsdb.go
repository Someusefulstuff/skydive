/*
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package probes

import (
	"fmt"
	"strings"
	"sync"

	"github.com/socketplane/libovsdb"

	"github.com/skydive-project/skydive/common"
	"github.com/skydive-project/skydive/config"
	"github.com/skydive-project/skydive/logging"
	"github.com/skydive-project/skydive/ovs"
	"github.com/skydive-project/skydive/topology"
	"github.com/skydive-project/skydive/topology/graph"
)

var (
	patchMetadata = graph.Metadata{"Type": "patch"}
)

type OvsdbProbe struct {
	sync.Mutex
	Graph           *graph.Graph
	Root            *graph.Node
	OvsMon          *ovsdb.OvsMonitor
	uuidToIntf      map[string]*graph.Node
	uuidToPort      map[string]*graph.Node
	intfPortQueue   map[string]*graph.Node
	portBridgeQueue map[string]*graph.Node
}

func (o *OvsdbProbe) OnOvsBridgeUpdate(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.OnOvsBridgeAdd(monitor, uuid, row)
}

func (o *OvsdbProbe) OnOvsBridgeAdd(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.Lock()
	defer o.Unlock()

	name := row.New.Fields["name"].(string)

	o.Graph.Lock()
	defer o.Graph.Unlock()

	bridge := o.Graph.LookupFirstNode(graph.Metadata{"UUID": uuid})
	if bridge == nil {
		bridge = o.Graph.NewNode(graph.GenID(), graph.Metadata{"Name": name, "UUID": uuid, "Type": "ovsbridge"})
		topology.AddOwnershipLink(o.Graph, o.Root, bridge, nil)
	}

	switch row.New.Fields["ports"].(type) {
	case libovsdb.OvsSet:
		set := row.New.Fields["ports"].(libovsdb.OvsSet)

		for _, i := range set.GoSet {
			u := i.(libovsdb.UUID).GoUUID

			port, ok := o.uuidToPort[u]
			if ok {
				if !topology.HaveOwnershipLink(o.Graph, bridge, port, nil) {
					topology.AddOwnershipLink(o.Graph, bridge, port, nil)
					topology.AddLayer2Link(o.Graph, bridge, port, nil)
				}
			} else {
				/* will be filled later when the port update for this port will be triggered */
				o.portBridgeQueue[u] = bridge
			}
		}

	case libovsdb.UUID:
		u := row.New.Fields["ports"].(libovsdb.UUID).GoUUID

		port, ok := o.uuidToPort[u]
		if ok {
			if !topology.HaveOwnershipLink(o.Graph, bridge, port, nil) {
				topology.AddOwnershipLink(o.Graph, bridge, port, nil)
				topology.AddLayer2Link(o.Graph, bridge, port, nil)
			}
		} else {
			/* will be filled later when the port update for this port will be triggered */
			o.portBridgeQueue[u] = bridge
		}
	}
}

func (o *OvsdbProbe) OnOvsBridgeDel(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.Graph.Lock()
	defer o.Graph.Unlock()

	bridge := o.Graph.LookupFirstNode(graph.Metadata{"UUID": uuid})
	if bridge != nil {
		o.Graph.DelNode(bridge)
	}
}

func (o *OvsdbProbe) OnOvsInterfaceAdd(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.Lock()
	defer o.Unlock()

	var ofport int64
	switch row.New.Fields["ofport"].(type) {
	case float64:
		ofport = int64(row.New.Fields["ofport"].(float64))
	default:
		return
	}

	var mac string
	switch row.New.Fields["mac_in_use"].(type) {
	case string:
		mac = row.New.Fields["mac_in_use"].(string)
	}

	var index int64
	if i, ok := row.New.Fields["ifindex"]; ok {
		switch row.New.Fields["ifindex"].(type) {
		case float64:
			index = int64(i.(float64))
		case libovsdb.OvsSet:
			set := row.New.Fields["ifindex"].(libovsdb.OvsSet)
			if len(set.GoSet) > 0 {
				index = set.GoSet[0].(int64)
			}
		}
	}

	var itype string
	if t, ok := row.New.Fields["type"]; ok {
		itype = t.(string)
	}

	var driver string
	if d, ok := row.New.Fields["status"].(libovsdb.OvsMap).GoMap["driver_name"]; ok {
		driver = d.(string)
	}

	if driver == "" {
		// force the driver as it is not defined and we need it to delete properly
		switch itype {
		case "gre", "vxlan", "geneve", "patch":
			driver = "openvswitch"
		default:
			// need to be sure that we have the driver
			return
		}
	}

	name := row.New.Fields["name"].(string)

	o.Graph.Lock()
	defer o.Graph.Unlock()

	intf := o.Graph.LookupFirstNode(graph.Metadata{"UUID": uuid})
	if intf == nil {
		lm := graph.Metadata{"Name": name, "Driver": driver}
		if index > 0 {
			lm["IfIndex"] = index
		}
		if mac != "" {
			lm["MAC"] = mac
		}

		// added before by netlink ?
		if intf = o.Graph.LookupFirstNode(lm); intf != nil {
			o.Graph.AddMetadata(intf, "UUID", uuid)
		}
	}

	if intf == nil {
		intf = o.Graph.NewNode(graph.GenID(), graph.Metadata{"Name": name, "UUID": uuid, "Driver": driver})
	}

	tr := o.Graph.StartMetadataTransaction(intf)
	defer tr.Commit()

	if ofport > 0 {
		tr.AddMetadata("OfPort", ofport)
	}

	if index > 0 {
		tr.AddMetadata("IfIndex", index)
	}

	if mac != "" {
		tr.AddMetadata("MAC", mac)
	}

	if itype != "" {
		tr.AddMetadata("Type", itype)
	}

	extIds := row.New.Fields["external_ids"].(libovsdb.OvsMap)
	for k, v := range extIds.GoMap {
		tr.AddMetadata("ExtID/"+k.(string), v.(string))
	}

	o.uuidToIntf[uuid] = intf

	switch itype {
	case "gre", "vxlan", "geneve":
		m := row.New.Fields["options"].(libovsdb.OvsMap)
		if ip, ok := m.GoMap["local_ip"]; ok {
			tr.AddMetadata("LocalIP", ip.(string))
		}
		if ip, ok := m.GoMap["remote_ip"]; ok {
			tr.AddMetadata("RemoteIP", ip.(string))
		}
		m = row.New.Fields["status"].(libovsdb.OvsMap)
		if iface, ok := m.GoMap["tunnel_egress_iface"]; ok {
			tr.AddMetadata("TunEgressIface", iface.(string))
		}
		if carrier, ok := m.GoMap["tunnel_egress_iface_carrier"]; ok {
			tr.AddMetadata("TunEgressIfaceCarrier", carrier.(string))
		}

	case "patch":
		m := row.New.Fields["options"].(libovsdb.OvsMap)
		if p, ok := m.GoMap["peer"]; ok {

			peerName := p.(string)

			peer := o.Graph.LookupFirstNode(graph.Metadata{"Name": peerName, "Type": "patch"})
			if peer != nil {
				if !topology.HaveLayer2Link(o.Graph, intf, peer, patchMetadata) {
					topology.AddLayer2Link(o.Graph, intf, peer, patchMetadata)
				}
			} else {
				// lookup in the intf queue
				for _, peer := range o.uuidToIntf {
					if name, _ := peer.GetFieldString("Name"); name == peerName && !topology.HaveLayer2Link(o.Graph, intf, peer, patchMetadata) {
						topology.AddLayer2Link(o.Graph, intf, peer, patchMetadata)
					}
				}
			}
		}
	}

	/* set pending interface for a port */
	if port, ok := o.intfPortQueue[uuid]; ok {
		if !topology.HaveLayer2Link(o.Graph, port, intf, nil) {
			topology.AddLayer2Link(o.Graph, port, intf, nil)
		}
		delete(o.intfPortQueue, uuid)
	}
}

func (o *OvsdbProbe) OnOvsInterfaceUpdate(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.OnOvsInterfaceAdd(monitor, uuid, row)
}

func (o *OvsdbProbe) OnOvsInterfaceDel(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.Lock()
	defer o.Unlock()

	intf, ok := o.uuidToIntf[uuid]
	if !ok {
		return
	}

	o.Graph.Lock()
	defer o.Graph.Unlock()

	// do not delete if not an openvswitch interface
	if driver, _ := intf.GetFieldString("Driver"); driver == "openvswitch" {
		o.Graph.DelNode(intf)
	}

	delete(o.uuidToIntf, uuid)
}

func (o *OvsdbProbe) OnOvsPortAdd(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.Lock()
	defer o.Unlock()

	o.Graph.Lock()
	defer o.Graph.Unlock()

	port, ok := o.uuidToPort[uuid]
	if !ok {
		port = o.Graph.NewNode(graph.GenID(), graph.Metadata{
			"UUID": uuid,
			"Name": row.New.Fields["name"].(string),
			"Type": "ovsport",
		})
		o.uuidToPort[uuid] = port
	}

	// bond mode
	if mode, ok := row.New.Fields["bond_mode"]; ok {
		switch mode.(type) {
		case string:
			o.Graph.AddMetadata(port, "BondMode", mode.(string))
		}
	}

	// lacp
	if lacp, ok := row.New.Fields["lacp"]; ok {
		switch lacp.(type) {
		case string:
			o.Graph.AddMetadata(port, "LACP", lacp.(string))
		}
	}

	// vlan tag
	if tag, ok := row.New.Fields["tag"]; ok {
		switch tag.(type) {
		case libovsdb.OvsSet:
			set := tag.(libovsdb.OvsSet)
			if len(set.GoSet) > 0 {
				o.Graph.AddMetadata(port, "Vlans", set.GoSet)
			}
		case float64:
			o.Graph.AddMetadata(port, "Vlans", int(tag.(float64)))
		}
	}

	switch row.New.Fields["interfaces"].(type) {
	case libovsdb.OvsSet:
		set := row.New.Fields["interfaces"].(libovsdb.OvsSet)

		for _, i := range set.GoSet {
			u := i.(libovsdb.UUID).GoUUID
			intf, ok := o.uuidToIntf[u]
			if ok {
				if !topology.HaveLayer2Link(o.Graph, port, intf, nil) {
					topology.AddLayer2Link(o.Graph, port, intf, nil)
				}
			} else {
				/* will be filled later when the interface update for this interface will be triggered */
				o.intfPortQueue[u] = port
			}
		}
	case libovsdb.UUID:
		u := row.New.Fields["interfaces"].(libovsdb.UUID).GoUUID
		intf, ok := o.uuidToIntf[u]
		if ok {
			if !topology.HaveLayer2Link(o.Graph, port, intf, nil) {
				topology.AddLayer2Link(o.Graph, port, intf, nil)
			}
		} else {
			/* will be filled later when the interface update for this interface will be triggered */
			o.intfPortQueue[u] = port
		}
	}

	/* set pending port of a container */
	if bridge, ok := o.portBridgeQueue[uuid]; ok {
		if !topology.HaveLayer2Link(o.Graph, bridge, port, nil) {
			topology.AddLayer2Link(o.Graph, bridge, port, nil)
		}
		delete(o.portBridgeQueue, uuid)
	}
}

func (o *OvsdbProbe) OnOvsPortUpdate(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.OnOvsPortAdd(monitor, uuid, row)
}

func (o *OvsdbProbe) OnOvsPortDel(monitor *ovsdb.OvsMonitor, uuid string, row *libovsdb.RowUpdate) {
	o.Lock()
	defer o.Unlock()

	port, ok := o.uuidToPort[uuid]
	if !ok {
		return
	}

	o.Graph.Lock()
	defer o.Graph.Unlock()

	o.Graph.DelNode(port)

	delete(o.uuidToPort, uuid)
}

func (o *OvsdbProbe) Start() {
	o.OvsMon.StartMonitoring()
}

func (o *OvsdbProbe) Stop() {
	o.OvsMon.StopMonitoring()
}

func NewOvsdbProbe(g *graph.Graph, n *graph.Node, p string, t string) *OvsdbProbe {
	mon := ovsdb.NewOvsMonitor(p, t)
	mon.ExcludeColumn("statistics")

	o := &OvsdbProbe{
		Graph:           g,
		Root:            n,
		uuidToIntf:      make(map[string]*graph.Node),
		uuidToPort:      make(map[string]*graph.Node),
		intfPortQueue:   make(map[string]*graph.Node),
		portBridgeQueue: make(map[string]*graph.Node),
		OvsMon:          mon,
	}
	o.OvsMon.AddMonitorHandler(o)

	return o
}

func NewOvsdbProbeFromConfig(g *graph.Graph, n *graph.Node) *OvsdbProbe {
	address := config.GetConfig().GetString("ovs.ovsdb")

	var protocol string
	var target string

	if strings.HasPrefix(address, "unix://") {
		target = strings.TrimPrefix(address, "unix://")
		protocol = "unix"
	} else if strings.HasPrefix(address, "tcp://") {
		target = strings.TrimPrefix(address, "tcp://")
		protocol = "tcp"
	} else {
		// fallback to the original address format addr:port
		sa, err := common.ServiceAddressFromString("ovs.ovsdb")
		if err != nil {
			logging.GetLogger().Errorf("Configuration error: %s", err.Error())
			return nil
		}

		protocol = "tcp"
		target = fmt.Sprintf("%s:%d", sa.Addr, sa.Port)
	}

	return NewOvsdbProbe(g, n, protocol, target)
}
