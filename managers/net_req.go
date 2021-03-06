// vi: sw=4 ts=4:
/*
 ---------------------------------------------------------------------------
   Copyright (c) 2013-2015 AT&T Intellectual Property

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at:

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
 ---------------------------------------------------------------------------
*/


/*
	Mnemonic:	net_req.go
	Abstract:	Functions that manage net_req struct.
	Date:		16 November 2014
	Author:		E. Scott Daniels

	Mods:		27 Feb 2015 - Changes to make steering work with lazy update.
				31 Mar 2015 - Changes to provide a force load of all VMs into the network graph.
*/

package managers

import (
	"github.com/att/gopkgs/ipc"
)

type Net_vm  struct {
	name	*string
	id		*string			// openstack assigned id
	ip4		*string			// openstack assigned ip address
	ip6		*string			// openstack assigned ip address
	phost	*string			// phys host where vm is running
	mac		*string			// MAC
	gw		*string			// the gateway associated with the VM (if known)
	fip		*string			// floating ip
	gwmap	map[string]*string // the gateway information associated with the VM (obsolete)
}

/*
	Create a vm insertion structure. Not a good idea to create a nil named structure, but
	we'll allow it and subs in the ip4 value as its name if provided, otherwise the string unnamed.
*/
func Mk_netreq_vm( name *string, id *string, ip4 *string, ip6 *string, phost *string, mac *string, gw *string, fip *string, gwmap map[string]*string )  ( np *Net_vm ) {
	if name == nil {
		if ip4 != nil {				// no name, use ip4 if there
			name = ip4
		} else {
			unv := "unnamed"
			name = &unv
		}
	}

	np = &Net_vm {
		name: name,
		id: id,
		ip4: ip4,
		ip6: ip6,
		phost: phost,
		mac: mac,
		gw: gw,
		fip: fip,
		gwmap: gwmap,			// we assume the map is ours to keep
	}

	return
}

/*
	Returns all values except the gateway map.
*/
func (vm *Net_vm) Get_values( ) ( name *string, id *string, ip4 *string, ip6 *string, gw *string, phost *string, mac *string, fip *string ) {
	if vm == nil {
		return
	}

	return vm.name, vm.id, vm.ip4, vm.ip6, vm.phost, vm.gw, vm.mac, vm.fip
}

/*
	Returns the map.
*/
func (vm *Net_vm) Get_gwmap() ( map[string]*string ) {
	return vm.gwmap
}

/*
	Replaces the name in the struct with the new value if nv isn't nil;
*/
func (vm *Net_vm) Put_name( nv *string ) {
	if vm != nil  && nv != nil {
		vm.name = nv
	}
}

/*
	Replaces the id with the new value
*/
func (vm *Net_vm) Put_id( nv *string ) {
	if vm != nil {
		vm.id = nv
	}
}

/*
	Replaces the id with the new value
*/
func (vm *Net_vm) Put_ip4( nv *string ) {
	if vm != nil {
		vm.ip4 = nv
	}
}

/*
	Replaces the id with the new value
*/
func (vm *Net_vm) Put_ip6( nv *string ) {
	if vm != nil {
		vm.ip6 = nv
	}
}

/*
	Replace the physical host with the supplied value.
*/
func (vm *Net_vm) Put_phost( nv *string ) {
	if vm != nil {
		vm.phost = nv
	}
}

/*
	Send the vm struct to network manager as an insert to it's maps
*/
func  (vm *Net_vm) Add2graph( nw_ch chan *ipc.Chmsg ) {

	msg := ipc.Mk_chmsg( )
	msg.Send_req( nw_ch, nil, REQ_ADD, vm, nil )		
}

/*
	Output in human readable form.
*/
func (vm *Net_vm) To_str() ( string ) {
	if vm == nil {
		return ""
	}

	str := ""
	if vm.name != nil {
		str = str + *vm.name + " "
	} else {
		str = str + "<nil> "
	}
	if vm.id != nil {
		str = str + *vm.id + " "
	} else {
		str = str + "<nil> "
	}
	if vm.ip4 != nil {
		str = str + *vm.ip4 + " "
	} else {
		str = str + "<nil> "
	}
	if vm.ip6 != nil {
		str = str + *vm.ip6 + " "
	} else {
		str = str + "<nil> "
	}
	if vm.phost != nil {
		str = str + *vm.phost + " "
	} else {
		str = str + "<nil> "
	}
	if vm.gw != nil {
		str = str + *vm.gw + " "
	} else {
		str = str + "<nil> "
	}
	if vm.mac != nil {
		str = str + *vm.mac + " "
	} else {
		str = str + "<nil> "
	}
	if vm.fip != nil {
		str = str + *vm.fip
	} else {
		str = str + "<nil>"
	}

	return str
}	
