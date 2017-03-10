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

	Mnemonic:	agent
	Abstract:	Manages everything associated with agents. Listens on the well known channel
				for requests from other tegu threads, and manages a separate data channel
				for agent input (none expected at this time.

	Date:		30 April 2014
	Author:		E. Scott Daniels

	Mods:		05 May 2014 : Added ability to receive and process json data from the agent
					and the function needed to process the output from a map_mac2phost request.
					Added ability to send the map_mac2phost request to the agent.
				13 May 2014 : Added support for exit-dscp value.
				05 Jun 2014 : Fixed stray reference to net_sheep.
				29 Oct 2014 : Corrected potential core dump if agent msg received is less than
					100 bytes.
				17 Jun 2105 : Added oneway reservation support.
				16 Nov 2105 : Handle response from remote mirror agents
*/

package managers

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/att/gopkgs/bleater"
	"github.com/att/gopkgs/clike"
	"github.com/att/gopkgs/connman"
	"github.com/att/gopkgs/ipc"
	"github.com/att/gopkgs/jsontools"
)

// ----- structs used to bundle into json commands

type action struct {			// specific action
	Atype	string				// something like map_mac2phost, or intermed_queues
	Aid		uint32				// action id to be sent in the response
	Data	map[string]string	// generic data - probably json directly from the outside world, but who knows
	Hosts	[]string			// list of hosts to apply the action to
	Dscps	string				// space separated list of dscp values
	Fdata	[]string			// flowmod command data
	Qdata	[]string			// queue parms
}

type agent_cmd struct {			// overall command
	Ctype	string
	Actions []action
}

/*
	Manage things associated with a specific agent
*/
type agent struct {
	id		string
	jcache	*jsontools.Jsoncache				// buffered input resulting in 'records' that are complete json blobs
}

type agent_data struct {
	agents	map[string]*agent					// hash for direct index (based on ID string given to the session)
	agent_list []*agent							// sequential index into map that allows easier round robin access for sendone
	aidx	int									// next spot in index for round robin sends
}

/*
	Generic struct to unpack json received from an agent
*/
type agent_msg struct {
	Ctype	string			// command type -- should be response, ack, nack etc.
	Rtype	string			// type of response (e.g. map_mac2phost, or specific id for ack/nack)
	Rdata	[]string		// response stdout data
	Edata	[]string		// response error data
	State	int				// if an ack/nack some state information
	Vinfo	string			// agent version (debugging mostly)
	Rid		uint32			// original request id
}

/*
	Build the agent list from the map. The agent list is a 'sequential' list of all currently
	connected agents which affords us an easy means to roundrobin through them.
*/
func (ad *agent_data) build_list( ) {
	ad.agent_list = make( []*agent, len( ad.agents ) )
	i := 0
	for _, a := range ad.agents {
		ad.agent_list[i] = a
		i++
	}

	if ad.aidx >= i {			// wrap if list shrank and we point beyond it
		ad.aidx = 0
	}
}

/*
	Build an agent and add to our list of agents.
*/
func (ad *agent_data) Mk_agent( aid string ) ( na *agent ) {

	na = &agent{}
	na.id = aid
	na.jcache = jsontools.Mk_jsoncache()

	ad.agents[na.id] = na
	ad.build_list( )

	return
}

/*
	Send the message to one agent. The agent is selected using the current
	index in the agent_data so that it effectively does a round robin.
*/
func (ad *agent_data) send2one( smgr *connman.Cmgr,  msg string ) {
	l := len( ad.agents )
	if l <= 0 {
		return
	}

	smgr.Write( ad.agent_list[ad.aidx].id, []byte( msg ) )
	ad.aidx++
	if ad.aidx >= l {
		if l > 1 {
			ad.aidx = 1		// skip the long running agent if more than one agent connected
		} else {
			ad.aidx = 0
		}
	}
}

/*
	Send the message to one agent. The agent is selected using the current
	index in the agent_data so that it effectively does a round robin.
*/
func (ad *agent_data) sendbytes2one( smgr *connman.Cmgr,  msg []byte ) {
	l := len( ad.agents )
	if l <= 0 {
		return
	}

	smgr.Write( ad.agent_list[ad.aidx].id,  msg )
	ad.aidx++
	if ad.aidx >= l {
		if l > 1 {
			ad.aidx = 1		// skip the long running agent if more than one agent connected
		} else {
			ad.aidx = 0
		}
	}
}
/*
	Send the message to the designated 'long running' agent (lra); the
	agent that has been designated to handle all long running tasks
	that are not time sensitive (such as intermediate queue setup/checking).
*/
func (ad *agent_data) sendbytes2lra( smgr *connman.Cmgr,  msg []byte ) {
	l := len( ad.agents )
	if l <= 0 {
		return
	}

	smgr.Write( ad.agent_list[0].id,  msg )
}

/*
	Send the message to the designated 'long running' agent (lra); the
	agent that has been designated to handle all long running tasks
	that are not time sensitive (such as intermediate queue setup/checking).
*/
func (ad *agent_data) send2lra( smgr *connman.Cmgr,  msg string ) {
	l := len( ad.agents )
	if l <= 0 {
		return
	}

	smgr.Write( ad.agent_list[0].id,  []byte( msg ) )
}

/*
	Send the message to all agents.
*/
func (ad *agent_data) send2all( smgr *connman.Cmgr,  msg string ) {
	am_sheep.Baa( 2, "sending %d bytes", len( msg ) )
	for id := range ad.agents {
		smgr.Write( id, []byte( msg ) )
	}
}

/*
	Deal with incoming data from an agent. We add the buffer to the cahce
	(all input is expected to be json) and attempt to pull a blob of json
	from the cache. If the blob is pulled, then we act on it, else we
	assume another buffer or more will be coming to complete the blob
	and we'll do it next time round.
*/
func ( a *agent ) process_input( buf []byte ) {
	var (
		req	agent_msg		// unpacked message struct
	)

	a.jcache.Add_bytes( buf )
	jblob := a.jcache.Get_blob()						// get next blob if ready
	for ; jblob != nil ; {
    	err := json.Unmarshal( jblob, &req )           // unpack the json

		if err != nil {
			am_sheep.Baa( 0, "ERR: unable to unpack agent_message: %s  [TGUAGT000]", err )
			am_sheep.Baa( 2, "offending json: %s", string( buf ) )
		} else {
			am_sheep.Baa( 1, "%s/%s received from agent", req.Ctype, req.Rtype )

			switch( req.Ctype ) {					// "command type"
				case "response":					// response to a request
					if req.State == 0 {
						switch( req.Rtype ) {
							case "map_mac2phost":
								msg := ipc.Mk_chmsg( )
								msg.Send_req( nw_ch, nil, REQ_MAC2PHOST, req.Rdata, nil )		// send into network manager -- we don't expect response

							case "mirrorwiz":
								// Stuff the response back in the mirror object - quick and dirty and probably not "right"
								save_mirror_response( req.Rdata, req.Edata )

							default:
								am_sheep.Baa( 2, "WRN:  success response data from agent was ignored for: %s  [TGUAGT001]", req.Rtype )
								if am_sheep.Would_baa( 2 ) {
									am_sheep.Baa( 2, "first few ignored messages from response:" )
									for i := 0; i < len( req.Rdata ) && i < 10; i++ {
										am_sheep.Baa( 2, "[%d] %s", i, req.Rdata[i] )
									}
								}
						}
					} else {
						switch( req.Rtype ) {
							case "bwow_fmod":
								am_sheep.Baa( 1, "ERR: oneway bandwidth flow-mod failed; check agent logs for details  [TGUAGT006]" )
								for i := 0; i < len( req.Rdata ) && i < 20; i++ {
									am_sheep.Baa( 1, "  [%d] %s", i, req.Rdata[i] )
								}

							default:
								am_sheep.Baa( 1, "WRN: response messages for failed command were not interpreted: %s  [TGUAGT002]", req.Rtype )
								for i := 0; i < len( req.Rdata ) && i < 20; i++ {
									am_sheep.Baa( 2, "  [%d] %s", i, req.Rdata[i] )
								}
						}
					}

				default:
					am_sheep.Baa( 1, "WRN:  unrecognised command type type from agent: %s  [TGUAGT003]", req.Ctype )
			}
		}

		jblob = a.jcache.Get_blob()								// get next blob if the buffer completed one and contains a second
	}

	return
}

//-------- request builders -----------------------------------------------------------------------------------------

/*
	Build a request to have the agent generate a mac to phost list and send it to one agent.
*/
func (ad *agent_data) send_mac2phost( smgr *connman.Cmgr, hlist *string ) {
	if hlist == nil || *hlist == "" {
		am_sheep.Baa( 2, "no host list, cannot request mac2phost" )
		return
	}

/*
	req_str := `{ "ctype": "action_list", "actions": [ { "atype": "map_mac2phost", "hosts": [ `
	toks := strings.Split( *hlist, " " )
	sep := " "
	for i := range toks {
		req_str += sep + `"` + toks[i] +`"`
		sep = ", "
	}

	req_str += ` ] } ] }`
*/

	msg := &agent_cmd{ Ctype: "action_list" }				// create command struct then convert to json
	msg.Actions = make( []action, 1 )
	msg.Actions[0].Atype = "map_mac2phost"
	msg.Actions[0].Hosts = strings.Split( *hlist, " " )
	jmsg, err := json.Marshal( msg )			// bundle into a json string

	if err == nil {
		am_sheep.Baa( 3, "sending mac2phost request: %s", jmsg )
		ad.sendbytes2lra( smgr, jmsg )						// send as a long running request
	} else {
		am_sheep.Baa( 1, "WRN: unable to bundle mac2phost request into json: %s  [TGUAGT004]", err )
		am_sheep.Baa( 2, "offending json: %s", jmsg )
	}
}

/*
	Build a request to cause the agent to drive the setting of queues and fmods on intermediate bridges.
*/
func (ad *agent_data) send_intermedq( smgr *connman.Cmgr, hlist *string, dscp *string ) {
	if hlist == nil || *hlist == "" {
		return
	}

	msg := &agent_cmd{ Ctype: "action_list" }				// create command struct then convert to json
	msg.Actions = make( []action, 1 )
	msg.Actions[0].Atype = "intermed_queues"
	msg.Actions[0].Hosts = strings.Split( *hlist, " " )
	msg.Actions[0].Dscps = *dscp

	jmsg, err := json.Marshal( msg )			// bundle into a json string

	if err == nil {
		am_sheep.Baa( 1, "sending intermediate queue setup request: hosts=%s dscp=%s", *hlist, *dscp )
		ad.sendbytes2lra( smgr, jmsg )						// send as a long running request
	} else {
		am_sheep.Baa( 0, "WRN: creating json intermedq command failed: %s  [TGUAGT005]", err )
	}
}

// ---------------- utility ------------------------------------------------------------------------

/*
	Accepts a string of space separated dscp values and returns a string with the values
	appropriately shifted so that they can be used by the agent in a flow-mod command.  E.g.
	a dscp value of 40 is shifted to 160.
*/
func shift_values( list string ) ( new_list string ) {
	new_list = ""
	sep := ""
	toks := strings.Split( list, " " )

	for i := range toks {
		n := clike.Atoi( toks[i] )
		new_list += fmt.Sprintf( "%s%d", sep, n<<2 )
		sep = " "
	}

	return
}

// ---------------- main agent goroutine -----------------------------------------------------------

func Agent_mgr( ach chan *ipc.Chmsg ) {
	var (
		port	string = "29055"						// port we'll listen on for connections
		adata	*agent_data
		host_list string = ""
		dscp_list string = "46 26 18"				// list of dscp values that are used to promote a packet to the pri queue in intermed switches
		refresh int64 = 60
		iqrefresh int64 = 1800							// intermediate queue refresh (this can take a long time, keep from clogging the works)
	)

	adata = &agent_data{}
	adata.agents = make( map[string]*agent )

	am_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	am_sheep.Set_prefix( "agentmgr" )
	tegu_sheep.Add_child( am_sheep )					// we become a child so that if the master vol is adjusted we'll react too

														// suss out config settings from our section
	if cfg_data["agent"] != nil {
		if p := cfg_data["agent"]["port"]; p != nil {
			port = *p
		}
		if p := cfg_data["agent"]["verbose"]; p != nil {
			am_sheep.Set_level( uint( clike.Atoi( *p ) ) )
		}
		if p := cfg_data["agent"]["refresh"]; p != nil {
			refresh = int64( clike.Atoi( *p ) )
		}
		if p := cfg_data["agent"]["iqrefresh"]; p != nil {
			iqrefresh = int64( clike.Atoi( *p ) )
			if iqrefresh < 1800 {
				am_sheep.Baa( 1, "iqrefresh in configuration file is too small, set to 1800 seconds" )
				iqrefresh = 1800
			}
		}
	}
	if cfg_data["default"] != nil {						// we pick some things from the default section too
		if p := cfg_data["default"]["pri_dscp"]; p != nil {			// list of dscp (diffserv) values that match for priority promotion
			dscp_list = *p
			am_sheep.Baa( 1, "dscp priority list from config file: %s", dscp_list )
		} else {
			am_sheep.Baa( 1, "dscp priority list not in config file, using defaults: %s", dscp_list )
		}
	}

	dscp_list = shift_values( dscp_list )				// must shift values before giving to agent

														// enforce some sanity on config file settings
	am_sheep.Baa( 1,  "agent_mgr thread started: listening on port %s", port )

	tklr.Add_spot( 2, ach, REQ_MAC2PHOST, nil, 1 );  					// tickle once, very soon after starting, to get a mac translation
	tklr.Add_spot( 10, ach, REQ_INTERMEDQ, nil, 1 );		  			// tickle once, very soon, to start an intermediate refresh asap
	tklr.Add_spot( refresh, ach, REQ_MAC2PHOST, nil, ipc.FOREVER );  	// reocurring tickle to get host mapping
	tklr.Add_spot( iqrefresh, ach, REQ_INTERMEDQ, nil, ipc.FOREVER );  	// reocurring tickle to ensure intermediate switches are properly set

	sess_chan := make( chan *connman.Sess_data, 1024 )					// channel for comm from agents (buffers, disconns, etc)
	smgr := connman.NewManager( port, sess_chan );


	for {
		select {							// wait on input from either channel
			case req := <- ach:
				req.State = nil				// nil state is OK, no error

				am_sheep.Baa( 3, "processing request %d", req.Msg_type )

				switch req.Msg_type {
					case REQ_NOOP:						// just ignore -- acts like a ping if there is a return channel

					case REQ_SENDALL:					// send request to all agents
						if req.Req_data != nil {
							adata.send2all( smgr,  req.Req_data.( string ) )
						}

					case REQ_SENDLONG:					// send a long request to one agent
						if req.Req_data != nil {
							adata.send2one( smgr,  req.Req_data.( string ) )
						}

					case REQ_SENDSHORT:					// send a short request to one agent (round robin)
						if req.Req_data != nil {
							adata.send2one( smgr,  req.Req_data.( string ) )
						}

					case REQ_MAC2PHOST:					// send a request for agent to generate  mac to phost map
						if host_list != "" {
							adata.send_mac2phost( smgr, &host_list )
						}

					case REQ_CHOSTLIST:					// a host list from fq-manager
						if req.Req_data != nil {
							host_list = *(req.Req_data.( *string ))
						}

					case REQ_INTERMEDQ:
						req.Response_ch = nil
						if host_list != "" {
							adata.send_intermedq( smgr, &host_list, &dscp_list )
						}

				}

				am_sheep.Baa( 3, "processing request finished %d", req.Msg_type )			// we seem to wedge in network, this will be chatty, but may help
				if req.Response_ch != nil {				// if response needed; send the request (updated) back
					req.Response_ch <- req
				}


			case sreq := <- sess_chan:		// data from a connection or TCP listener
				switch( sreq.State ) {
					case connman.ST_ACCEPTED:		// newly accepted connection; no action

					case connman.ST_NEW:			// new connection
						a := adata.Mk_agent( sreq.Id )
						am_sheep.Baa( 1, "new agent: %s [%s]", a.id, sreq.Data )
						if host_list != "" {											// immediate request for this
							adata.send_mac2phost( smgr, &host_list )
							adata.send_intermedq( smgr, &host_list, &dscp_list )
						}

					case connman.ST_DISC:
						am_sheep.Baa( 1, "agent dropped: %s", sreq.Id )
						if _, not_nil := adata.agents[sreq.Id]; not_nil {
							delete( adata.agents, sreq.Id )
						} else {
							am_sheep.Baa( 1, "did not find an agent with the id: %s", sreq.Id )
						}
						adata.build_list()			// rebuild the list to drop the agent

					case connman.ST_DATA:
						if _, not_nil := adata.agents[sreq.Id]; not_nil {
							cval := 100
							if len( sreq.Buf ) < 100 {						// don't try to go beyond if chop value too large
								cval = len( sreq.Buf )
							}
							am_sheep.Baa( 2, "data: [%s]  %d bytes received:  first 100b: %s", sreq.Id, len( sreq.Buf ), sreq.Buf[0:cval] )
							adata.agents[sreq.Id].process_input( sreq.Buf )
						} else {
							am_sheep.Baa( 1, "data from unknown agent: [%s]  %d bytes ignored:  %s", sreq.Id, len( sreq.Buf ), sreq.Buf )
						}
				}
		}			// end select
	}
}
