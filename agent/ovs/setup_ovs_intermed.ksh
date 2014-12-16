#!/usr/bin/env ksh
# vim: sw=4 ts=4:

#	Mnemonic:	setup_ovs_intermed
#	Abstract:	This sets up generic queues and flow-mods on intermediate OVS bridges
#				(e.g. br-tun br-eth* and br-ex, or any bridge that is NOT br-int or 
#				br-rl,  such that:
#					1) a queue set containing a priority queue (q1)
#						and a best effort queue (q0) is created	
#					2) there is a flow-mod that matches on packets with the diffserv
#						bit set and cause the traffic to be put onto queue1
#					3) The queue set is attached to all interfaces on the switch
#
#				We do this by pulling all switch/port information from ovs and then
#				generating a data set that create_ovs_queues can digest. Then we simply
#				invoke create_ovs_queues to do the hard work. The process of generating
#				the queue data also identifies the bridge names and we install a flow
#				mod into each that does the promoting of the packet to the proper queue
#				when the diffserv bit is set.  The flow mod has NO timeout and must be
#				explictly deleted if it needs not to exist.
#
#				It is safe to run this script periodically in order to ensure that the 
#				queues and flow-mod(s) are in place. Running and reestablihsing the 
#				queues requires the same overhead as testing to see if they are set, so
#				there's no need to check for their existence first.  
#
#				A note about the flow-mods generated by this script:
#				In order to 'chain' our flow-mods with those generated by openstack, we 
#				set our priority value high so that we match when the dscp value is one 
#				in the list.  The matching rule will set the queue (generic 1) and then 
#				drive a sister rule in table 91 which flips a bit in the metadta. The 
#				control is returned to the main rule which then resubmits for table 0
#				to match the openstack rule or default.  The metadata bit prevents our 
#				main rule from matching when we resubmit table 0.  Sadly, setting the metadata 
#				value must be done after the resubmit or OVS will not accept the flow mod
#				which is why we must push a sister rule in table 91.
#
#				As a final part of the setup we must ensure that all GRE ports on br-tun are
#				configured to promote the tos data from the inner packet into the header
#				used as the packet flows through the tunnel.  
#
#				This has been extended and now does some non-intermediate bridge setup
#				in addition to the above functions:  
#
#				1) Sets up fmods on br-int (non-intermediate bride) such  that DSCP values for
#				non-reservation traffic will be set to 0. The rule depends on meta data that 
#				might be set by other reservation flow-mods using a series of alternate tables
#				within OVS.  Specifically tables 94 and 98 are set up by this script on br-int.
#				Should the table values need to change they can be overridden by setting the 
#				environment variables QL_M4_TABLE (94 table) and QL_M8_TABLE (98) table. 
#
#				2) Flow-mods are added to br-int which facilitate the ingress rate limiting
#				ability in a GRE environment (it's impossible to set usage caps on patch
#				interfaces as are set up by Openstack).  Two flow-mods are created to achieve
#				this:
#					a) maches traffic matched by a higher priority reservation flow-mod (marked
#					by metadata and outputs the packets on the veth connection to br-rl
#					b) matches all traffic received from br-rl and outputs it on the port
#					assocated with the patch-tun interface. 
#				The br-rl and the veth connection are both established by the ql_setup_rl 
#				script which are invoked by this script. 
#
#				Message prefix for this script is QLTSOM followed by three digits. 
#	
#	Author:		E. Scott Daniels
#	Date: 		02 May 2014
#
#	Mods:		07 May 2014 - Added ability to support multiple diffserv values. 
#				12 May 2014 - Changed printf to avoid %d limit on min/max values.
#				15 May 2014 - To correct issues with chaining fmods to openstack generated fmods.
#				16 May 2014 - To set resubmit allowing us to chain rules without matching our rule recursively.
#				30 Jul 2014 - Added setting of tos inheritence for gre-tunnel ports.    (hbdAKD)
#				27 Aug 2014 - Added br-int dropping rules. 
#				28 Aug 2014 - Changed alternate table to 91 to avoid collision with openstack.
#				02 Sep 2014 - Rouge applications which set values to Q-Lite DSCP values will have
#								their traffic modified such that the DSCP value is set to 0. If -D
#								given on the command line, then these rules are not written.
#				22 Sep 2014 - To _never_ set fmods on br-rl as that's our rate limiting 'loop' and should
#								not see any of the flow-mods set by this script. Added call to ql_setup_irl
#								to setup and configure the ingress rate limiting bridge and related
#								flow-mods.
#				05 Oct 2014 - Added better error checking round the irl settings.
#				07 Oct 2014 - Bug fix 227 - prevent intermediate fmod replacement from happening.
#				10 Nov 2014 - Added connect timeout to ssh calls
#				17 Nov 2014	- Added timeouts on ssh commands to prevent "stalls" as were observed in pdk1.
#				20 Nov 2014 - Now accepts a minimum parameter and sets default minimum to 500K with the
#								intent of setting quantum to ~6K.
#				21 Nov 2014 - Changed to deal with the duplicate MAC addresses (bonded interfaces).
#				04 Dec 2014 - Added a constant string to identify the target host in failure messages.
# ----------------------------------------------------------------------------------------------------------
#
#  Some OVS QoS and Queue notes....
# 	the ovs queue is a strange beast with confusing implementation. There is a QoS 'table' which defines 
# 	one or more entries with each entry consisting of a set of queues.  Each port can be assigned one QoS 
# 	entry which serves to define the queue(s) for the port.  
# 	An odd point about the QoS entry is that the entry itself caries a max-rate setting (no min-rate), but
# 	it's not clear in any documentation as to how this is applied.  It could be a default applied to all 
# 	queues such that only the min-rate for the queue need be set, or it could be used as a hard max-rate
# 	limit for all queues on the port in combination. 
#
# 	Further, it seems to cause the switch some serious heartburn if the controller sends a flowmod to the 
# 	switch which references a non-existant queue, this in turn causes some serious stack dumping in the
# 	controller.


# ----------------------------------------------------------------------------------------------------------
trap "cleanup" 1 2 3 15 EXIT

# ensure tmp files go away if we die
function cleanup
{
	trap - EXIT
	rm -f /tmp/PID$$.*
}

#
# expand a value suffixed with G, GiB or g into a 'full' value (XiB or x are powers of 2, while X is powers of 10)
function expand
{
	case $1 in 
		*KiB)		echo $(( ${1%K*} * 1024 ));;
		*k)			echo $(( ${1%k*} * 1024 ));;
		*K)			echo $(( ${1%?} * 1000 ));;

		*MiB)		echo $(( ${1%M*} * 1024 * 1024 ));;
		*m)			echo $(( ${1%m*} * 1024 * 1024 ));;
		*M)			echo $(( ${1%?} * 1000000 ));;

		*GiB)		echo $(( ${1%G*} * 1024 * 1024 * 1024 ));;
		*g)			echo $(( ${1%g*} * 1024 * 1024 * 1024 ));;
		*G)			echo $(( ${1%?} * 1000000000 ));;

		*)			echo $1;;
	esac
}

function logit
{
	echo "$(date "+%s %Y/%m/%d %H:%M:%S") $argv0: $@" >&2
}

function usage
{
	cat <<-endKat


	version 1.1/18274
	usage: $argv0 [-b bride(s)] [-d difserv] [-e max-tput] [-l log-file] [-n] [-h hostname] [-v]

	  -b sets the bridge(s) to affect (default br-ex and br-tun). Space separated if there are more 
	     than one.  Regardless of the bridges listed, this script _always_ sets the ineritence
	     for tos on br-tun unless no execute (-n) is given.
	  -D Do not write dropping flow-mods

	endKat
}
# --------------------------------------------------------------------------------------------------------------

argv0=${0##*/}

if [[ $argv0 == "/"* ]]
then
	PATH="$PATH:${argv0%/*}"		# ensure the directory that contains us is in the path
fi

one_gbit=1000000000

entry_max_rate=$(expand 10G)
purge_ok=1

ssh_opts="-o ConnectTimeout=2 -o StrictHostKeyChecking=no -o PreferredAuthentications=publickey"
verbose=0
forreal=1
allow_reset=1			# -D sets to 0 to prevent writing the dscp reset flowmods
allow_irl=1				# -I turns off irl configuration
delete_data=0
						# both host values set when -h given on the command line
rhost=""				# given on commands like ovs_sp2uuid (will be -h foo)
thost=$(hostname)		# target host defaults here, but overridden by -h
diffserv=184			# diffserv bit to set; voice (46), default
min=$( expand 500K )	# this generally sets the quantum to about 6000
max=$( expand 10G )
#bridges="br-tun br-ex" 	# default bridges to affect; -b overrides
bridges="" 				# default to all found in the ovs listing; -b will override if needed

while [[ $1 == -* ]]
do
	case $1 in 
		-b)	bridges+="$2 "; shift;;
		-d)	diffserv="$2"; shift;;
		-D)	allow_reset=0;;						# do not write the dscp reset flowmods
		-e)	entry_max_rate=$( expand $2 ); shift;;
		-h)	
			thost=$2; 							# override target host
			rhost="-h $2"; 						# set option for any ovs_sp2uuid calls etc
			ssh_host="ssh -n $ssh_opts $2" 		# CAUTION: this MUST have -n since we don't redirect stdin to ssh
			shift
			;;

		-I)	allow_irl=0;;

		-l)  log_file=$2; shift;;
		-m)	min=$( expand $2 ); shift;;
		-n)	no_exec_str="no exec: "; noexec="-n";;
		-v) vflag="-v"; verbose=1;;

		-\?)	usage
				exit 0
				;;

		*)	echo "unrecognised option: $1" >&2
			usage
			exit 1
			;;
	esac
	shift
done

if [[ -n $log_file ]]			# force stdout/err to a known place; helps when executing from the agent
then
	exec >$log_file 2>&1
fi

if (( $(id -u) != 0 ))
then
	sudo="sudo"					# must use sudo for the ovs-vsctl commands
fi


if (( allow_irl ))				# do this first so that we can snag the assigned rate limit port from ovs_sp2uuid output
then
	logit "setting up ingress rate limiting bridge and flow-mods   [OK]"
	ql_setup_irl $noexec $rhost
	# errors are written  to stderr by ql_setup, and return status is ignored as failure to set up 
	# the rl bridge is not harmful to, and should not prevent, the remainder of the chores.
else
	logit "-I given; ingress rate limiting setup was skipped  [OK]"
fi

fmod_data=/tmp/PID$$.fdata
rl_data=/tmp/PID$$.rdata
queue_data=/tmp/PID$$.qdata
>$fmod_data							# must exist and enusre it's empty

# generate the data that will be given to create_ovs_queues
ovs_sp2uuid -a $rhost any |	awk \
	-v thost="${thost%%.*}" \
	-v sudo="$sudo" \
	-v max_rate=$entry_max_rate \
	-v min=$min \
	-v max=$max \
	-v fmod_data=$fmod_data \
	-v rl_data=$rl_data \
	'
	BEGIN {
		qlsep = "";
	}

	#switch: 00003ad52e019f44 012ed53a-a352-449f-843e-6ca367201824 br-int
	/^switch: / && NF > 2 { 			# bridge on the switch
		cur_switch = $4;

		n = split( $2, a, "" )			# ovs spits out the dpid w/o :s so we need to add them back
		#mac = ""						# we use the uuid now as macs can be duplicated
		#for( i = 1; i < n-1; i += 2 )
		#	mac = mac a[i] a[i+1] ":"
		#mac = mac a[i] a[i+1]
		#swmac[cur_switch] = mac;
		swuuid[cur_switch] = $3;		# macs can be duplicated it seems -- grrr.

		pidx[cur_switch] = 0;

		if( cur_switch != "br-int" )
			slist[cur_switch] = 1
		next;
	}

	#port: 99e3b26b-1bb2-48b0-9468-e080140978a5 336 tap4e46815f-a4 fa:de:ad:c0:ee:20 4e46815f-a46b-4133-8f44-3747a3a57de3
	/^port: / && NF > 1 {					# collect port data allowing us to map port/queue to a uuid
		if( $4 != cur_switch )				# dont set one for the "internal" interface
		{
			ports[cur_switch,pidx[cur_switch]] = $3;
			pidx[cur_switch]++;
		}

		if( cur_switch == "br-int" )
		{
			if( $4 == "qosirl0" )						# qos ingress rate limit port into br-int
				rl_port = $3 + 0;									
			else
				if( $4 == "patch-tun" )					# needed to specifically route traffic from rl bridge directly to tun patch interface
					tun_port = $3 + 0;
		}
		next;
	}

	# generate the data for create_ovs_queues that applies to the named bridge
	# output now uses the uuid rather than the switch MAC address as it seems those can be dups.
	function pswitch( name )
	{
		if( pidx[name] > 0 )
			printf( "%s\n", name ) >fmod_data;		# capture names of brides we saw
		else
			printf( "did not find: %s\n", name ) >"/dev/fd/2";

		for( i = 0; i < pidx[name]; i++ )
		{
			printf( "%s/%d,priority,1,%.0f,%.0f,200\n", swuuid[name], ports[name,i], min, max );		# priority queue -- lower pri value == higher priority
			printf( "%s/%d,besteff,0,%.0f,%.0f,1500\n", swuuid[name], ports[name,i], min, max );		# best effort with wide limits and low priority
		}
	}

	END {
		for( s in slist )
			pswitch( s );

		if( tun_port > 0  &&  rl_port > 0 )						# found both the rate limit and tunnel ports
			printf( "%d %d\n", rl_port, tun_port ) >rl_data;		# save them for later
	}
' >$queue_data

if [[ -s $rl_data ]]									# there is a rate limiting port on br-int, set the flow-mods to and from it
then
	head -1 $rl_data | read rl_port tun_port junk		# port on br-int where rl-data pipe is attached 
	if [[ -n $rl_port && -n $tun_port ]]
	then
		logit "setting rate limiting flow-mods from br-rl port $rl_port to patch-tun $tun_port and into br-rl	[OK]"
																												# use a cookie different than all others as we delete all that match the cookie
		irl_rc=0
		# bug fix 227 send_ovs_fmod $noexec $rhost -t 0 -p 190 --match  --action del 0xdeaf br-int							# must delete the preivous ones on the off chance that the veth port changed
		# irl_rc=$(( irl_rc += $? ))

		# because we check for rate limiting before trying to set these, it is safe to invoke with -I and not duplicate the check
		send_ovs_fmod $noexec $rhost -I -t 0 -p 999 --match -m 0x0/0x08 -i $tun_port --action -R ",98" -R ",0" -N add 0xdeaf br-int	# in from tunnel; set high meta flag (0x08) to prevent pushing into br-rl
		irl_rc=$(( irl_rc += $? ))

		send_ovs_fmod $noexec $rhost -I -t 0 -p 190 --match -i $rl_port  --action -o $tun_port add 0xdeaf br-int		# reservation f-mods are used to set the queue, so match after
		irl_rc=$(( irl_rc += $? ))

		send_ovs_fmod $noexec $rhost -I -T ${QL_M8_TABLE:-98} -t 0  --match --action -m 0x8/0x8  -N  add 0xbeef br-int	# cannot set meta before resub, so set in alternate table
		irl_rc=$(( irl_rc += $? ))

		send_ovs_fmod $noexec $rhost -I -t 0 -p 180 --match -m 0x02  --action -o $rl_port add 0xdeaf br-int 	#  packet matched outbound reservation rule; CAUTION: the match is a _hard_ value match not a mask match
		irl_rc=$(( irl_rc += $? ))

		if (( irl_rc != 0 ))
		then
			logit "CRI: unable to set one or more ingress rate limiting flow-mods. target-host: ${rhost#* } [FAIL]	[QLTSOM000]"
		fi
	else
		logit "WRN: no rl_port or patch-tun port information was found; br-rl related flow-mods not set for target-host: ${rhost#* } [QLTSOM001]" 	# these are warnings because it might be OK not to have br-rl active
	fi
else
	logit "WRN: ingress rate limiting flow mods not set -- OVS data missing from target-host: ${rhost#* }    [QLTSOM002]"
fi

if (( verbose ))
then
	while read buf
	do
		logit "queue data: $buf"
	done <$queue_data
fi

if [[ -z $bridges ]]
then
	while read br 
	do
		if [[ $br != "br-rl" ]]			# should be trapped later, but doesn't hurt to prevent inclusion here too
		then
			bridges+="$br "
		fi
	done <$fmod_data
fi

if [[ -s $queue_data ]]
then
	for br in $bridges
	do
		#if ! coq_test.ksh $vflag -l "$br" $noexec $rhost $queue_data >/tmp/PID$$.coq		# -k keeps unreferenced queues
		if ! create_ovs_queues $vflag -l "$br" $noexec $rhost $queue_data >/tmp/PID$$.coq		# -k keeps unreferenced queues
		then
			logit "CRI: unable to set one or more ovs queues on target-host: ${rhost#* }   [FAIL] [QLTSOM003]"
			cat /tmp/PID$$.coq >&2
			rm -f /tmp/PID$$.*
			exit 1
		fi
	done
else
	logit "no queue setup data was generated  [WARN]" 
	logit "to verify: ovs_sp2uuid -a $rhost any"
fi

# CAUTION:  the order that the action parameters are supplied is VERY important!! Don't mess with them 
#			unless you know what you are doing. It is also important that neither rule we generate for
#			each dscp value has any kind of 'send' (normal, output, enqueue) action. 
#
rc=0
while read b		# for each bridge listed set a flow mod to push marked packets onto the priority queue
do
	if [[ $b != "br-rl" ]]				# never assign anything to the rate limiting bridge
	then
		for dscp in $diffserv				# might be multiple values, space separated
		do
			send_ovs_fmod $noexec $rhost -t 0  --match -m 0/1 -T $dscp --action -q 1 -R ",91" -R ",0" -N  add 0xbeef $b			# set queue and drive rule in tabl 1 then drive table 0 for ostack
			lrc=0
			rc=$(( lrc + $? ))
			send_ovs_fmod $noexec $rhost -T 91 -t 0  --match -T $dscp --action -m 1/1  -N  add 0xbeef $b				# cannot set meta before resub, so set in alternate table
			rc=$(( lrc + $? ))
	
			if (( lrc == 0  ))
			then
				logit "${no_exec_str}intermediate flow-mods were set on ${rhost% } dscp=$dscp bridge=$b	[OK]"
			else
				logit "CRI: unable to set flow-mod for bridge=$b  dscp=$dscp on target-host: ${rhost#* }   [FAIL]  [QLTSOM004]"
				rc=1
			fi
		done
	fi
done <$fmod_data


# Configure OVS to promote dscp value into gre header
# Send request to remote ovs for bridge information that we'll suss out gre data from.
# Then we'll execute the command that we generated back on the same host.
timeout 15 $ssh_host sudo ovs-vsctl show | awk -v sudo=$sudo '
	BEGIN {
		snarf = 0;
		pidx = 0
	}
	
	function emit( ) {
		if( gre  && iname != "" ) {
			plist[pidx++] = iname
			iname = ""
			gre = 0
		}
	}

	{ gsub( "\"", "", $0 ) }
	/Bridge br-tun/ { snarf = 1; next; }
	/Bridge / { snarf = 0; next; }
	snarf == 0 { next; }
	
	/Port / { gre = 0; iname = ""; next; }
	/Interface/ { iname = $NF; emit(); next; }
	/type: gre/ { gre = 1; emit( ); next; }
		
	END {
		if( pidx > 0 ) {
			printf( "%s ovs-vsctl ", sudo )
			for( i = 0; i < pidx; i++ ) {
				printf( " -- set Interface %s options:tos=inherit", plist[i] )
			}
			printf( "\n" );
		}
	}
' | read cmd

if [[ -n $cmd ]]						# promote command generated
then
	if [[ -z $no_exec_str ]]			# ok to run it if string is empty
	then
		if ! $ssh_host $cmd
		then
			logit "CRI: unable to set tos inheritence. target-host: ${rhost#* }    [FAIL]  [QLTSOM005]"
			rc=1
		fi
	else
		$no_exec_str $ssh_host $cmd		# no exec mode; just echo things
	fi
else
	if (( verbose ))
	then
		logit "no gre ports found on host ($rhost); no promotion of tos set"
	fi
fi

if (( allow_reset ))		# write the f-mods that drop the DSCP values from traffic that have tegu dscp markings and were not set by a tegu f-mod (meta & 0x02 == 0)
then
	send_ovs_fmod $noexec $rhost -T ${QL_M4_TABLE:-94} -t 0  --match --action -m 0x4/0x4  -N  add 0xbeef br-int			# cannot set meta before resub, so set in alternate table

	if [[ ! -f /etc/tegu/no_dscp_reset ]]			# safety valve
	then
		# CAUTION:  the meta value match is a _hard_ value of zero, not a mask match so we don't turn off any packet that matched a reservation fmod or inbound traffic
		if ! send_ovs_fmod $rhost $noexec -t 0 -p 10 --match  -m 0x00 --action -T 0  -R ",${QL_M4_TABLE:-94}" -R ",0" -N add 0xfeed br-int  # turn off dscp, submit for meta mark, then resubmit to 0 
		then
			logit "CRI: unable to set dscp reset rule for ${rhost#* }   [FAIL] [QOSSOM006]"
			rc=1
		fi
	else
		logit "no dropping flow-mods written, /etc/tegu/no_dscp_drop existed"
	fi
else
	logit "no dropping flow-mods written -D was set"
fi

rm -f /tmp/PID$$.*
exit $rc