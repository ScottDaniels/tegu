#!/bin/ksh
# vi: sw=4 ts=4 noexpandtab:
#
# ---------------------------------------------------------------------------
#   Copyright (c) 2013-2015 AT&T Intellectual Property
#
#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at:
#
#       http://www.apache.org/licenses/LICENSE-2.0
#
#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.
# ---------------------------------------------------------------------------

#
#       Name:      tegu_add_mirror
#       Usage:     tegu_add_mirror [-o<options>] [-v] <name> <port1>[,<port2>...] <output> [<vlan>]
#       Abstract:  This script starts a mirror named <name> on openvswitch.
#
#                  The port list for the mirror is named by <port1>, <port2>, etc. which
#                  must be a comma-separated list of ports that already exist on br-int.
#                  The ports can be named either by a UUID (OVS or neutron) or MAC.
#                  If a MAC is provided, this script translates to an OVS UUID.
#
#                  <output> directs where the output of the mirror goes.  There are three
#                  possibilities:
#                  1. If <output> is vlan:nnn such that 1 <= n <= 4095, it is the VLAN
#                  number for the output VLAN.
#                  2. If <output> is an IPv4 or IPv6 address, then a port is created that
#                  acts as one end of a GRE tunnel to the IP address.  IPv6 addresses MUST
#                  be fully specified (with 7 ":"s) in order to distinguish them from MACs.
#                  3. If <output> is a UUID (or MAC) of an existing port on br-int,
#                  then output is directed to that port.
#
#                  If <vlan> (optional) is specified, and is a comma-separated list of VLAN
#                  IDs, it is used to select the VLANs whose traffic should be mirrored.
#                  That is, a "select-vlan=$vlan" is added to the call to openvswitch
#
#                  The -v switch causes all openvswitch commands to be echoed.
#
#                  The only currently valid option is -oflowmod, to create a flowmod based mirror.
#
#                  If succesful, this command prints the mirror name on exit.
#
#       Author:    Robert Eby
#       Date:      04 February 2015
#
#       Mods:      04 Feb 2015 - created
#                  27 Apr 2015 - allow IPv6 for <output> GRE address
#                  25 Jun 2015 - Corrected PATH.
#                  15 Sep 2015 - Remove extra copyright
#                  17 Sep 2015 - Add ability to use neutron UUID for ports
#                  19 Oct 2015 - Add options:in_key to GRE ports to allow multiple GRE ports.
#                                Allow mirrors on bridges other than br-int
#                  16 Nov 2015 - Put mirror name in all error messages
#                  23 Nov 2015 - Add -oflowmod option processing
#                  09 Jan 2016 - Handle VLAN=-1 case in -oflowmod option processing.
#                                Allow df_default=(true|false) and df_inherit=(true|false) in options
#                  18 Jan 2016 - Fix flowmod mirrors so they resubmit (in case vlans need to be rewritten)
#					03 Jun 2016 - Map vlans when setting up flow-mod based mirrors.
#					01 Jul 2016 - Fix the map to go both directions.
#					15 Jul 2016 - Correct missing return in vlan-id translation funciton.
#					01 Sep 2016 - Correct the declaration of the inbound vlan map array
#

# --------------------------------------------------------------------------------------------------------------

#	Looks at the flow-mods on the indicated bridge and creates a map from external vlan id to the
#	internal id.  We make the following assumptions:
#		1) neutron flow-mods all have cookie 0x0 and ours do not
#		2) the flow-mod output from ovs lists the match criteria first
#		   followed by the action information.
#
#	As an example:
#		If a VM shows vlan 2 from an ovs_sp2uuid perspective, but
#		neutron has set up vlan 3200 for that traffic to go out of br-int on, then
#		looking up vlan id 2 will result in 3200.
function map_ib_vlans
{
	sudo ovs-ofctl dump-flows ${1:-br-int} | grep cookie=0x0,.*mod_vlan |sed 's/.*dl_vlan=//; s/ .*mod_vlan_vid:/ /; s/,.*//' | while read from to junk
	do
		ib_vlan_map[$to]=$from
		ib_vlan_map[$from]=$to
	done
}

#	Given the VM's local vlan ID, map it to the vlan id that will be used
#	outside of this environment (the vlan id that we expect packets to be
#	marked with upon arrival. If there is no translation (this is an environment
#	where vlan translation isn't done) then the same vlan id passed in is
#	returned
function xlate_vlan
{
	if (( $1 < 1 ))			# prevent accidents if array is not associative
	then
		echo $1
		return
	fi

	typeset xvid=${ib_vlan_map[${1:--1}]}
	if [[ -n $xvid ]]
	then
		echo $xvid
		return
	fi

	echo $1
}

function valid_ip4
{
	echo "$1." | grep -E -q "^([0-9]{1,3}[\.]){4}$"
	return $?
}

function valid_ip6
{
	case "$1" in
	::*)
		echo "$1" | grep -E -q "(:[0-9a-fA-F]{1,4}){1,7}$"
		;;
	*::)
		echo "$1" | grep -E -q "^([0-9a-fA-F]{1,4}:){1,7}"
		;;
	*::*)
		echo "$1:" | grep -E -q "^([0-9a-fA-F]{0,4}:){1,8}$"
		;;
	*)
		echo "$1:" | grep -E -q "^([0-9a-fA-F]{1,4}:){8}$"
		;;
	esac
	return $?
}

function valid_mac
{
	echo "$1:" | grep -E -q "^([0-9a-fA-F]{1,2}:){6}$"
	return $?
}

function valid_port
{
	for t in $brports
	do
		[ "$1" == "$t" ] && return 0
	done
	return 1
}

function translatemac
{
	ovs_sp2uuid -a | awk -v mac=$1 '/^port/ && $5 == mac { print $2 }'
}

function translateuuid
{
	ovs_sp2uuid -a | awk -v uuid=$1 '/^port/ && ($2 == uuid || $6 == uuid) { print $2 }'
}

function findbridge
{
	ovs_sp2uuid -a | awk -v uuid=$1 '
		/^switch/ { br = $4 }
		/^port/ && $2 == uuid { print br }'
}

function option_set
{
	echo $options | tr ' ' '\012' | grep $1 > /dev/null
	return $?
}

function usage
{
	echo "usage: tegu_add_mirror [-o<options>] [-v] name port1[,port2,...] output [vlan]" >&2
}

# Preliminaries
PATH=$PATH:/sbin:/usr/bin:/bin		# must pick up agent augmented path
typeset -A ib_vlan_map				# maps local vlans to the inbound vlan id that needs to appear on the flowmod
echo=:
options=
while [[ "$1" == -* ]]
do
	if [[ "$1" == "-v" ]]
	then
		echo=echo
		shift
	elif [[ "$1" == -o* ]]
	then
		options=`echo $1 | sed -e 's/^-o//' -e 's/,/ /g'`
		shift
	else
		usage
		exit 1
	fi
done
if [ $# -lt 3 -o $# -gt 4 ]
then
	usage
	exit 1
fi
if [ ! -x /usr/bin/ovs-vsctl ]
then
	echo "tegu_add_mirror: ovs-vsctl is not installed or not executable." >&2
	exit 2
fi

bridgename=br-int		# bridge will usually be br-int, but can be changed below
mirrorname=$1
ports=$2
output=$3
vlan=${4:-}
sudo=sudo
[ "`id -u`" == 0 ] && sudo=
id=`uuidgen -t`

# Check port list
$echo $sudo ovs-vsctl --columns=ports list bridge
brports=`$sudo ovs-vsctl --columns=ports list bridge 2>/dev/null | sed 's/.*://' | tr -d '[] ' | tr , '\012'`
if [ $? -ne 0 ]
then
	echo "tegu_add_mirror: $mirrorname: cannot list ports on openvswitch." >&2
	exit 2
fi

realports=""
for p in `echo $ports | tr , ' '`
do
	case "$p" in
	*-*-*-*-*)
		# Port UUID
		uuid=`translateuuid $p`
		if valid_port "$uuid"
		then
			realports="$realports,$uuid"
			bridgename=$(findbridge $uuid)
		else
			echo "tegu_add_mirror: $mirrorname: there is no port with UUID=$p on this machine." >&2
			exit 2
		fi
		;;

	*:*:*:*:*:*)
		# MAC addr
		uuid=`translatemac $p`
		if valid_port "$uuid"
		then
			realports="$realports,$uuid"
			bridgename=$(findbridge $uuid)
		else
			echo "tegu_add_mirror: $mirrorname: there is no port with MAC=$p on this machine." >&2
			exit 2
		fi
		;;

	*)
		echo "tegu_add_mirror: $mirrorname: port $p is invalid (must be a UUID or a MAC)." >&2
		exit 2
		;;
	esac
done
realports=`echo $realports | sed 's/^,//'`

map_ib_vlans $bridgename			# build a vlan translation map of inbound vlan ids to local vlan ids

# Check output type
case "$output" in
vlan:[0-9]+)
	outputtype=vlan
	output=`echo $output | sed s/vlan://`
	;;

*.*.*.*)
	if valid_ip4 "$output"
	then
		outputtype=gre
		remoteip=$output
	else
		echo "tegu_add_mirror: $mirrorname: $output is not a valid IPv4 address." >&2
		exit 2
	fi
	;;

*-*-*-*-*)
	# Output port specified by UUID
	if valid_port "$output"
	then
		outputtype=port
	else
		echo "tegu_add_mirror: $mirrorname: there is no port with UUID=$output on this machine." >&2
		exit 2
	fi
	;;

*:*)
	# Could be either a MAC or IPv6 address
	if valid_mac "$output"
	then
		# MAC addr
		uuid=`translatemac $output`
		if valid_port "$uuid"
		then
			outputtype=port
			output="$uuid"
		else
			echo "tegu_add_mirror: $mirrorname: there is no port with MAC=$output on this machine." >&2
			exit 2
		fi
	else
		if valid_ip6 "$output"
		then
			outputtype=gre
			remoteip=$output
		else
			echo "tegu_add_mirror: $mirrorname: $output is not a valid IPv6 address." >&2
			exit 2
		fi
	fi
	;;

*)
	echo "tegu_add_mirror: $mirrorname: $output is not a valid output destination." >&2
	exit 2
	;;
esac

# Check VLANs (if any)
for v in `echo $vlan | tr , ' '`
do
	if [ "$v" -lt 0 -o "$v" -gt 4095 ]
	then
		echo "tegu_add_mirror: $mirrorname: vlan $v is invalid (must be >= 0 and <= 4095)." >&2
		exit 2
	fi
done

# Generate arguments to ovs-vsctl
mirrorargs="select_src_port=$realports select_dst_port=$realports"
[ -n "$vlan" ] && mirrorargs="$mirrorargs select-vlan=$vlan"

case "$outputtype" in
gre)
	greportname=gre-$mirrorname
	key=$(echo $mirrorname | sed -e 's/mir-//' -e 's/_.$//')
	key=$((16#$key))
	if option_set flowmod
	then
		# Flow mod based mirror - create a GRE port, then mirror the $realports to it
		$echo $sudo ovs-vsctl \
			add-port $bridgename $greportname \
			-- set interface $greportname type=gre options:remote_ip=$remoteip options:in_key=$key
		$sudo ovs-vsctl \
			add-port $bridgename $greportname \
			-- set interface $greportname type=gre options:remote_ip=$remoteip options:in_key=$key

		# determine GRE port num, mirrored port num, mirrored MAC and vlan
		ovs_sp2uuid -a > /tmp/tam.$$
		CONST="ovs-ofctl -O OpenFlow10,OpenFlow11,OpenFlow12,OpenFlow13 add-flow $bridgename"
		GREPORT=$(grep $greportname < /tmp/tam.$$ | cut -d' ' -f3)
		for port in $(echo $realports | tr , ' ')
		do
			MIRRORPORT=$(grep $port < /tmp/tam.$$ | cut -d' ' -f3)
			MIRRORVLAN=$(grep $port < /tmp/tam.$$ | cut -d' ' -f7)
			 MIRRORMAC=$(grep $port < /tmp/tam.$$ | cut -d' ' -f5)

			MIRRORVLAN=$( xlate_vlan $MIRRORVLAN )						# translate to external vlan id if vlan translation is in effect
			if [ "$MIRRORVLAN" -gt 0 -a "$MIRRORVLAN" -lt 4095 ]
			then
				RULES="dl_vlan=$MIRRORVLAN,dl_dst=$MIRRORMAC"
			else
				RULES="dl_dst=$MIRRORMAC"
			fi
			$echo $sudo $CONST "cookie=0xfaad,priority=100,metadata=0/1,${RULES},action=set_field:0x01->metadata,output:$GREPORT,resubmit(,0)"
			      $sudo $CONST "cookie=0xfaad,priority=100,metadata=0/1,${RULES},action=set_field:0x01->metadata,output:$GREPORT,resubmit(,0)"
			$echo $sudo $CONST "cookie=0xfaad,priority=100,metadata=0/1,in_port=$MIRRORPORT,action=set_field:0x01->metadata,output:$GREPORT,resubmit(,0)"
			      $sudo $CONST "cookie=0xfaad,priority=100,metadata=0/1,in_port=$MIRRORPORT,action=set_field:0x01->metadata,output:$GREPORT,resubmit(,0)"
		done
		rm -f /tmp/tam.$$
	else
		# Normal OVS mirror
		$echo $sudo ovs-vsctl \
			add-port $bridgename $greportname \
			-- set interface $greportname type=gre options:remote_ip=$remoteip options:in_key=$key \
			-- --id=@p get port $greportname \
			-- --id=@m create mirror name=$mirrorname $mirrorargs output-port=@p \
			-- add bridge $bridgename mirrors @m
		$sudo ovs-vsctl \
			add-port $bridgename $greportname \
			-- set interface $greportname type=gre options:remote_ip=$remoteip options:in_key=$key \
			-- --id=@p get port $greportname \
			-- --id=@m create mirror name=$mirrorname $mirrorargs output-port=@p \
			-- add bridge $bridgename mirrors @m
	fi
	# Add user specified options to the GRE port
	for opt in 'df_default=true' 'df_default=false' 'df_inherit=true' 'df_inherit=false'
	do
		if option_set $opt
		then
			$echo $sudo ovs-vsctl set interface $greportname options:$opt
			$sudo ovs-vsctl set interface $greportname options:$opt
		fi
	done
	;;

vlan)
	$echo $sudo ovs-vsctl \
		--id=@m create mirror name=$mirrorname $mirrorargs output-vlan=$output \
		-- add bridge $bridgename mirrors @m
	$sudo ovs-vsctl \
		--id=@m create mirror name=$mirrorname $mirrorargs output-vlan=$output \
		-- add bridge $bridgename mirrors @m
	;;

port)
	$echo $sudo ovs-vsctl \
		-- --id=@p get port $output \
		-- --id=@m create mirror name=$mirrorname $mirrorargs output-port=@p \
		-- add bridge $bridgename mirrors @m
	$sudo ovs-vsctl \
		-- --id=@p get port $output \
		-- --id=@m create mirror name=$mirrorname $mirrorargs output-port=@p \
		-- add bridge $bridgename mirrors @m
	;;
esac

echo Mirror $mirrorname created on bridge $bridgename.
exit 0
