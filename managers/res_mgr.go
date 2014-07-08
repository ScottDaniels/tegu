// vi: sw=4 ts=4:

/*

	Mnemonic:	res_mgr
	Abstract:	Manages the inventory of reservations. 
				We expect it to be executed as a goroutine and requests sent via a channel.
	Date:		02 December 2013
	Author:		E. Scott Daniels

	CFG:		These config file variables are used when present:
					resmgr:ckpt_dir	- name of the directory where checkpoint data is to be kept (/var/lib/tegu)
									FWIW: /var/lib/tegu selected based on description: http://www.tldp.org/LDP/Linux-Filesystem-Hierarchy/html/var.html


	TODO:		need a way to detect when skoogie/controller has been reset meaning that all
				pushed reservations need to be pushed again. 

				need to check to ensure that a VM's IP address has not changed; repush 
				reservation if it has and cancel the previous one (when skoogi allows drops)

	Mods:		03 Apr 2014 (sd) : Added endpoint flowmod support.
				30 Apr 2014 (sd) : Enhancements to send flow-mods and reservation request to agents (Tegu-light)
				13 May 2014 (sd) : Changed to support exit dscp value in reservation.
				18 May 2014 (sd) : Changes to allow cross tenant reservations.
				19 May 2014 (sd) : Changes to support using destination floating IP address in flow mod.
				07 Jul 2014 (sd) : Changed to send network manager a delete message when deleteing a reservation
						rather than depending on the http manager to do that -- possible timing issues if we wait.
						Added support for reservation refresh.
*/

package managers

import (
	"bufio"
	//"errors"
	"fmt"
	"io"
	"os"
	//"strings"
	"time"

	"forge.research.att.com/gopkgs/bleater"
	"forge.research.att.com/gopkgs/clike"
	"forge.research.att.com/gopkgs/chkpt"
	"forge.research.att.com/gopkgs/ipc"
	"forge.research.att.com/tegu/gizmos"
)

//var (  NO GLOBALS HERE; use globals.go )

// --------------------------------------------------------------------------------------

/*
	Manages the reservation inventory
*/
type Inventory struct {
	cache	map[string]*gizmos.Pledge
	chkpt	*chkpt.Chkpt
}

// --- Private --------------------------------------------------------------------------

/*
	Encapsulate all of the current reservations into a single json blob.
*/
func ( i *Inventory ) res2json( ) (json string, err error) {
	var (
		sep 	string = ""
	)

	err = nil;
	json = `{ "reservations": [ `

	for _, p := range i.cache {
		if ! p.Is_expired( ) {
			json += fmt.Sprintf( "%s%s", sep, p.To_json( ) )
			sep = ","
		}
	}

	json += " ] }"

	return
}

/*
	Given a name, send a request to the network manager to translate it to an IP address.
*/
func name2ip( name *string ) ( ip *string ) {
	ip = nil

	ch := make( chan *ipc.Chmsg )	
	defer close( ch )									// close it on return
	msg := ipc.Mk_chmsg( )
	msg.Send_req( nw_ch, ch, REQ_GETIP, *name, nil )
	msg = <- ch
	if msg.State == nil {					// success
		ip = msg.Response_data.(*string)
	}

	return
}


/*
	Handles a response from the fq-manager that indicates the attempt to send a proactive ingress/egress flowmod to skoogi
	has failed.  Issues a warning to the log, and resets the pushed flag for the associated reservation.
*/
func (i *Inventory) failed_push( msg *ipc.Chmsg ) {
	fq_data := msg.Req_data.( []interface{} ) 		// data that was passed to fq_mgr (we'll dig out pledge id
	pid := fq_data[FQ_ID].( string )

	rm_sheep.Baa( 1, "WRN: proactive ie reservation failed, pledge marked unpushed: %s", pid )
	p := i.cache[pid]
	if p != nil {
		p.Reset_pushed()
	}
}

/*
	Checks to see if any reservations expired in the recent past (seconds). Returns true if there were. 
*/
func (i *Inventory) any_concluded( past int64 ) ( bool ) {

	for _, p := range i.cache {									// run all pledges that are in the cache
		if p != nil  &&  p.Concluded_recently( past ) {			// pledge concluded within past seconds
				return true
		}
	}

	return false
}

/*
	Checks to see if any reservations became active between (now - past) and the current time, or will become
	active between now and now + future seconds. (Past and future are number of seconds on either side of 
	the current time to check and are NOT timestamps.)
*/
func (i *Inventory) any_commencing( past int64, future int64 ) ( bool ) {

	for _, p := range i.cache {							// run all pledges that are in the cache
		if p != nil  &&  (p.Commenced_recently( past ) || p.Is_active_soon( future ) ) {	// will activate between now and the window
				return true
		}
	}

	return false
}

/*
	Runs the list of reservations in the cache and pushes out any that are about to become active (in the 
	next 15 seconds).  We push the reservation request to fq_manager which does the necessary formatting 
	and communication with skoogi.  With the new method of managing queues per reservation on ingress/egress 
	hosts, we now send to fq_mgr:
		h1, h2 -- hosts
		expiry
		switch/port/queue
	
	for each 'link' in the forward direction, and then we reverse the path and send requests to fq_mgr
	for each 'link' in the backwards direction.  Errors are returned to res_mgr via channel, but 
	asycnh; we do not wait for responses to each message generated here.

	Returns the number of reservations that were pushed.
	TODO: we need to handle the special case where both h1 and h2 attach to the same switch.
*/
func (i *Inventory) push_reservations( ch chan *ipc.Chmsg ) ( npushed int ) {
	var (
		fq_data	[]interface{}			// local work space to organise data for fq manager
		fq_sdata	[]interface{}		// copy of data at time message is sent so that it 'survives' after msg sent and this continues to update fq_data
		msg		*ipc.Chmsg
		ip2		*string					// the ip ad

		push_count	int = 0
		pend_count	int = 0
		pushed_count int = 0
	)

	fq_data = make( []interface{}, FQ_SIZE )
	fq_data[FQ_SPQ] = 1						// queue is unchanging for now

	rm_sheep.Baa( 2, "pushing reservations, %d in cache", len( i.cache ) )
	for rname, p := range i.cache {							// run all pledges that are in the cache
		if p != nil  &&  ! p.Is_pushed() {
			if p.Is_active() || p.Is_active_soon( 15 ) {	// not pushed, and became active while we napped, or will activate in the next 15 seconds
				fq_data[FQ_DSCP] = p.Get_dscp()
				h1, h2, p1, p2, _, expiry, _, _ := p.Get_values( )		// hosts, ports and expiry are all we need

				ip1 := name2ip( h1 )
				ip2 = name2ip( h2 )

				if ip1 != nil  &&  ip2 != nil {				// good ip addresses so we're good to go
					plist := p.Get_path_list( )				// each path that is a part of the reservation

					if push_count <= 0 {
						rm_sheep.Baa( 1, "pushing proactive reservations to skoogi; plistlen=%d", len( plist ) )
					}
					push_count++

					if p.Is_paused( ) {
						fq_data[FQ_EXPIRY] = time.Now().Unix( ) +  15	// if reservation shows paused, then we set the expiration to 15s from now  which should force the flow-mods out
					} else {
						fq_data[FQ_EXPIRY] = expiry						// set data constant to all requests for the path list
					}
					fq_data[FQ_ID] = rname
					fq_data[FQ_TPSPORT] = p1							// forward direction transport ports are h1==src h2==dest
					fq_data[FQ_TPDPORT] = p2
					timestamp := time.Now().Unix() + 16									// assume this will fall within the first few seconds of the reservation as we use it to find queue in timeslice

					for i := range plist { 												// for each path in the list, send fmod requests for each endpoint and each intermediate link, both forwards and backwards
						extip := plist[i].Get_extip()
						if extip != nil {
							fq_data[FQ_EXTIP] = *extip
						} else {
							fq_data[FQ_EXTIP] = ""
						}
						fq_data[FQ_EXTTY] = "-D"										// external reference is the destination for forward component

						epip, _ := plist[i].Get_h1().Get_addresses()					// forward first, from h1 -> h2 (must use info from path as it might be split)
						fq_data[FQ_IP1] = *epip
						epip, _ = plist[i].Get_h2().Get_addresses()
						fq_data[FQ_IP2] = *epip

						rm_sheep.Baa( 1, "res_mgr/push_reg: sending forward i/e flow-mods for path %d: %s h1=%s --> h2=%s ip1/2= %s/%s exp=%d", 
							i, rname, *h1, *h2, fq_data[FQ_IP1], fq_data[FQ_IP2], expiry )

						espq1, espq0 := plist[i].Get_endpoint_spq( &rname, timestamp )		// endpoints are saved h1,h2, but we need to process them in reverse here


						// ---- push flow-mods in the h1->h2 direction -----------
						if espq1 != nil {													// data flowing into h2 from h1 over h2 to switch connection (ep0 handled with reverse path)
							fq_data[FQ_DIR_IN] = true										// inbound to last host from egress switch
							fq_data[FQ_SPQ] = espq1
							fq_sdata = make( []interface{}, len( fq_data ) )
							copy( fq_sdata, fq_data )
							msg = ipc.Mk_chmsg()
							msg.Send_req( fq_ch, ch, REQ_IE_RESERVE, fq_sdata, nil )			// queue work to send to skoogi (errors come back asynch, successes do not generate response)
						}

						fq_data[FQ_SPQ] = plist[i].Get_ilink_spq( &rname, timestamp )			// send fmod to ingress switch on first link out from h1
						fq_data[FQ_DIR_IN] = false
						fq_sdata = make( []interface{}, len( fq_data ) )
						copy( fq_sdata, fq_data )
						msg = ipc.Mk_chmsg()
						msg.Send_req( fq_ch, ch, REQ_IE_RESERVE, fq_sdata, nil )				// queue work to send to skoogi (errors come back asynch, successes do not generate response)

						ilist := plist[i].Get_forward_im_spq( timestamp )						// get list of intermediate switch/port/qnum data in forward (h1->h2) direction
						//for ii := 0; ii < len( ilist ); ii++ {
						for ii := range ilist {
							fq_sdata = make( []interface{}, len( fq_data ) )
							copy( fq_sdata, fq_data )
							fq_sdata[FQ_SPQ] = ilist[ii]
							rm_sheep.Baa( 2, "send forward intermediate reserve: [%d] %s %d %d", ii, ilist[ii].Switch, ilist[ii].Port, ilist[ii].Queuenum )
							msg = ipc.Mk_chmsg()
							msg.Send_req( fq_ch, ch, REQ_IE_RESERVE, fq_sdata, nil )			// flow mod for each intermediate link in foward direction
						}


						// ---- push flow-mods in the h2->h1 direction -----------
						rev_rname := "R" + rname		// the egress link has an R(name) queue name
						fq_data[FQ_TPSPORT] = p2							// forward direction transport ports are h1==src h2==dest
						fq_data[FQ_TPDPORT] = p1

						fq_data[FQ_EXTTY] = "-S"							// external reference is the source for backward component
						epip, _ = plist[i].Get_h1().Get_addresses() 		// for egress and backward intermediates the dest is h1, so reverse them
						fq_data[FQ_IP2] = *epip

						epip, _ = plist[i].Get_h2().Get_addresses()
						fq_data[FQ_IP1] = *epip

						rm_sheep.Baa( 1, "res_mgr/push_reg: sending backward i/e flow-mods for path %d: %s h1=%s <-- h2=%s ip1-2=%s-%s %s %s exp=%d", 
							i, rev_rname, *h1, *h2, fq_data[FQ_IP1], fq_data[FQ_IP2], fq_data[FQ_EXTTY], fq_data[FQ_EXTIP], expiry )

						if espq0 != nil {											// data flowing into h1 from h2 over the h1-switch connection
							fq_data[FQ_DIR_IN] = true
							fq_data[FQ_SPQ] = espq0
							fq_sdata = make( []interface{}, len( fq_data ) )
							copy( fq_sdata, fq_data )
							msg = ipc.Mk_chmsg()
							msg.Send_req( fq_ch, ch, REQ_IE_RESERVE, fq_sdata, nil )			// queue work to send to skoogi (errors come back asynch, successes do not generate response)
						}

						fq_data[FQ_SPQ] = plist[i].Get_elink_spq( &rev_rname, timestamp )	// send res to egress switch on first link towards h1
						fq_data[FQ_DIR_IN] = false											// the rest are outbound 
						fq_sdata = make( []interface{}, len( fq_data ) )
						copy( fq_sdata, fq_data )
						msg = ipc.Mk_chmsg()
						msg.Send_req( fq_ch, ch, REQ_IE_RESERVE, fq_sdata, nil )		// queue work to send to skoogi

						ilist = plist[i].Get_backward_im_spq( timestamp )		// get list of intermediate switch/port/qnum data in backwards direction
						//for ii := 0; ii < len( ilist ); ii++ {
						for ii := range ilist {
							fq_data[FQ_SPQ] = ilist[ii]
							fq_sdata = make( []interface{}, len( fq_data ) )
							copy( fq_sdata, fq_data )
							rm_sheep.Baa( 2, "send backward intermediate reserve: [%d] %s %d %d", ii, ilist[ii].Switch, ilist[ii].Port, ilist[ii].Queuenum )
							msg = ipc.Mk_chmsg()
							msg.Send_req( fq_ch, ch, REQ_IE_RESERVE, fq_sdata, nil )			// flow mod for each intermediate link in backwards direction
						}
					}

					p.Set_pushed()				// safe to mark the pledge as having been pushed. 
				} 
			} else {
				pend_count++
			}
		} else {
			pushed_count++
		}
	}

	if push_count > 0 || rm_sheep.Would_baa( 2 ) {			// bleat if we pushed something, or if higher level is set in the sheep
		rm_sheep.Baa( 1, "push_reservations: %d pushed, %d pending, %d already pushed", push_count, pend_count, pushed_count )
	}

	return pushed_count
}

/*
	Turn pause mode on for all current reservations and reset their push flag so thta they all get pushed again.
*/
func (i *Inventory) pause_on( ) {
	for _, p := range i.cache {
		p.Pause( true )					// also reset the push flag		
	}
}

/*
	Turn pause mode off for all current reservations and reset their push flag so thta they all get pushed again.
*/
func (i *Inventory) pause_off( ) {
	for _, p := range i.cache {
		p.Resume( true )					// also reset the push flag		
	}
}

/*
	Run the set of reservations in the cache and write any that are not expired out to the checkpoint file.  
	For expired reservations, we'll delete them if they test positive for extinction (dead for more than 120
	seconds).
*/
func (i *Inventory) write_chkpt( ) {

	err := i.chkpt.Create( )
	if err != nil {
		rm_sheep.Baa( 0, "CRI: resmgr: unable to create checkpoint file: %s", err )
		return
	}

	for key, p := range i.cache {
		s := p.To_chkpt()		
		if s != "expired" {
			fmt.Fprintf( i.chkpt, "%s\n", s ) 					// we'll check the overall error state on close
		} else {
			if p.Is_extinct( 120 ) && p.Is_pushed( ) {			// if really old and extension was pushed, safe to clean it out
				rm_sheep.Baa( 1, "extinct reservation purged: %s", key )
				delete( i.cache, key )
			}
		}
	} 

	ckpt_name, err := i.chkpt.Close( )
	if err != nil {
		rm_sheep.Baa( 0, "CRI: resmgr: checkpoint write failed: %s: %s", ckpt_name, err )
	} else {
		rm_sheep.Baa( 1, "resmgr: checkpoint successful: %s", ckpt_name )
	}
}

/*
	Opens the filename passed in and reads the reservation data from it. The assumption is that records in 
	the file were saved via the write_chkpt() function and are json pledges.  We will drop any that 
	expired while 'sitting' in the file. 
*/
func (i *Inventory) load_chkpt( fname *string ) ( err error ) {
	var (
		rec		string
		nrecs	int = 0
		p		*gizmos.Pledge
		my_ch	chan	*ipc.Chmsg
		req		*ipc.Chmsg
	)

	err = nil
	my_ch = make( chan *ipc.Chmsg )
	defer close( my_ch )									// close it on return

	f, err := os.Open( *fname )
	if err != nil {
		return
	}
	defer	f.Close( )

	br := bufio.NewReader( f )
	for ; err == nil ; {
		nrecs++
		rec, err = br.ReadString( '\n' )
		if err == nil  {
			p = new( gizmos.Pledge )
			p.From_json( &rec )

			if  p.Is_expired() {
				rm_sheep.Baa( 1, "resmgr: ckpt_load: ignored expired pledge: %s", p.To_str() )
			} else {

				req = ipc.Mk_chmsg( )
				req.Send_req( nw_ch, my_ch, REQ_RESERVE, p, nil )
				req = <- my_ch									// should be OK, but the underlying network could have changed

				if req.State == nil {						// reservation accepted, add to inventory
					err = i.Add_res( p )
				} else {

					rm_sheep.Baa( 0, "ERR: resmgr: ckpt_laod: unable to reserve for pledge: %s", p.To_str() )
				}
			}
		}
	}

	if err == io.EOF {
		err = nil
	}

	rm_sheep.Baa( 1, "read %d records from checkpoint file: %s", nrecs, *fname )
	return
}

/*
	Given a host name, return all pledges that involve that host as a list. 
	Currently no error is detected and the list may be nill if there are no pledges.
*/
func (inv *Inventory) pledge_list(  vmname *string ) ( []*gizmos.Pledge, error ) {

	if len( inv.cache ) <= 0 {
rm_sheep.Baa( 1, ">>> <<<< bailing early -- no inventory?" )
		return nil, nil
	}

	plist := make( []*gizmos.Pledge, len( inv.cache ) )
	i := 0
	for _, p := range inv.cache {
		if p.Has_host( vmname ) {
rm_sheep.Baa( 1, "pledge matched: %s", *vmname )
			plist[i] = p
			i++
		} else {
rm_sheep.Baa(  1, "pledge did not match: %s %s", *vmname, p.To_json() )
}
	}

rm_sheep.Baa( 1, ">>> returning 0:%d", i )
	return plist[0:i], nil
}

// --- Public ---------------------------------------------------------------------------
/*
	constructor
*/
func Mk_inventory( ) (inv *Inventory) {

	inv = &Inventory { } 

	inv.cache = make( map[string]*gizmos.Pledge, 2048 )		// initial size is not a limit

	return
}

/*
	Stuff the pledge into the cache erroring if the pledge already exists.
*/
func (inv *Inventory) Add_res( p *gizmos.Pledge ) (state error) {
	state = nil
	id := p.Get_id()
	if inv.cache[*id] != nil {
		state = fmt.Errorf( "reservation already exists: %s", *id )
		return
	}

	inv.cache[*id] = p

	rm_sheep.Baa( 1, "resgmgr: added reservation: %s", p.To_chkpt() )
	return
}

/*
	Return the reservation that matches the name passed in provided that the cookie supplied
	matches the cookie on the reservation as well.  The cookie may be either the cookie that 
	the user supplied when the reservation was created, or may be the 'super cookie' admin
	'root' as you will, which allows access to all reservations.
*/
func (inv *Inventory) Get_res( name *string, cookie *string ) (p *gizmos.Pledge, state error) {
	
	state = nil
	p = inv.cache[*name]
	if p == nil {
		state = fmt.Errorf( "cannot find reservation: %s", *name )
		return
	}

	if ! p.Is_valid_cookie( cookie ) &&  *cookie != *super_cookie {
		rm_sheep.Baa( 2, "resgmgr: denied fetch of reservation: cookie supplied (%s) didn't match that on pledge %s", *cookie, *name )
		p = nil
		state = fmt.Errorf( "not authorised to access or delete reservation: %s", *name )
		return
	}

	rm_sheep.Baa( 2, "resgmgr:: fetched reservation: %s", p.To_str() )
	return
}

/*
	Looks for the named reservation and deletes it if found. The cookie must be either the 
	supper cookie, or the cookie that the user supplied when the reservation was created.
	Deletion is affected by reetting the expiry time on the pledge to now + a few seconds. 
	This will cause a new set of flow-mods to be sent out with an expiry time that will
	take them out post haste and without the need to send "delete" flow-mods out. 

	This function sends a request to the network manager to delete the related queues. This
	must be done here so as to prevent any issues with the loosely coupled management of 
	reservation and queue settings.  It is VERY IMPORTANT to delete the reservation from 
	the network perspective BEFORE the expiry time is reset.  If it is reset first then 
	the network splits timeslices based on the new expiry and queues end up dangling.
*/
func (inv *Inventory) Del_res( name *string, cookie *string ) (state error) {

	p, state := inv.Get_res( name, cookie )
	if p != nil {
		rm_sheep.Baa( 2, "resgmgr: deleted reservation: %s", p.To_str() )
		state = nil

		ch := make( chan *ipc.Chmsg )	
		defer close( ch )										// close it on return
		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, ch, REQ_DEL, p, nil )			// delete from the network point of view
		req = <- ch											// wait for response from network
		state = req.State

		p.Set_expiry( time.Now().Unix() + 15 )				// set the expiry to 15s from now which will force it out
		p.Reset_pushed()									// force push of flow-mods that reset the expiry
	} else {
		rm_sheep.Baa( 2, "resgmgr: unable to delete reservation: not found: %s", *name )
	}

	return
}


/*
	delete all of the reservations provided that the cookie is the super cookie. If cookie
	is a user cookie, then deletes all reservations that match the cookie.
*/
func (inv *Inventory) Del_all_res( cookie *string ) ( ndel int ) {
	var	(
		plist	[]*string			// we'll create a list to avoid deletion issues with range
		i		int
	)

	ndel = 0
	
	plist = make( []*string, len( inv.cache ) )
	for _, pledge := range inv.cache {
		plist[i] = pledge.Get_id()
		i++
	}

	for _, pname := range plist {
		rm_sheep.Baa( 2, "delete all attempt to delete: %s", *pname )
		err := inv.Del_res( pname,  cookie )
		if err == nil {
			ndel++;
			rm_sheep.Baa( 1, "delete all deleted reservation %s", *pname )
		} else {
			rm_sheep.Baa( 1, "delete all skipped reservation %s", *pname )
		}
	}

	rm_sheep.Baa( 1, "delete all deleted %d reservations %s", ndel )
	return
}


/*
	Pulls the reservation from the inventory. Similar to delete, but not quite the same.
	This will clone the pledge. The clone is expired and left in the inventory to force
	a reset of flowmods. The network manager is sent a request to delete the queues 
	assocaited with the path and the path is removed from the original pledge. The orginal
	pledge is returned so that it can be used to generate a new set of paths based on the 
	hosts, expiry and bandwidth requirements of the initial reservation. 

	Unlike the get/del functions, this is meant for internal support and does not
	require a cookie. 

	It is important to delete the reservation from the network manager point of view 
	BEFORE the expiry is reset. If expiry is set first then the network manager will
	cause queue timeslices to be split on that boundary leaving dangling queues. 
*/
func (inv *Inventory) yank_res( name *string ) ( p *gizmos.Pledge, state error) {

	state = nil
	p = inv.cache[*name]
	if p != nil {
		rm_sheep.Baa( 2, "resgmgr: yanked reservation: %s", p.To_str() )
		cp := p.Clone( *name + ".yank" )				// clone but DO NOT set conclude time until after network delete!

rm_sheep.Baa( 1, "cloned reservation: %s",  *cp.Get_id() )
		inv.cache[*name + ".yank"] = cp					// insert the cloned pledge into the inventory

		inv.cache[*name] = nil							// yank original from the list
		delete( inv.cache, *name )
		p.Set_path_list( nil )							// no path list for this pledge 	

rm_sheep.Baa( 1, "requesting delete" )
		ch := make( chan *ipc.Chmsg )	
		defer close( ch )									// close it on return
		req := ipc.Mk_chmsg( )
		req.Send_req( nw_ch, ch, REQ_DEL, cp, nil )			// delete from the network point of view
		req = <- ch											// wait for response from network
		state = req.State

														// now safe to set these
		cp.Set_expiry( time.Now().Unix() + 1 )			// force clone to be expired
		cp.Reset_pushed( )								// force it to go out again
	} else {
		state = fmt.Errorf( "no reservation with name: %s", *name )
		rm_sheep.Baa( 2, "resgmgr: unable to yank, no reservation with name: %s", *name )
	}

	return
}

//---- res-mgr main goroutine -------------------------------------------------------------------------------

/*
	Executes as a goroutine to drive the resevration manager portion of tegu. 
*/
func Res_manager( my_chan chan *ipc.Chmsg, cookie *string ) {

	var (
		inv	*Inventory
		msg	*ipc.Chmsg
		ckptd	string
		last_qcheck	int64				// time that the last queue check was made to set window
		queue_gen_type = REQ_GEN_EPQMAP
	)

	super_cookie = cookie				// global for all methods

	rm_sheep = bleater.Mk_bleater( 0, os.Stderr )		// allocate our bleater and attach it to the master
	rm_sheep.Set_prefix( "res_mgr" )
	tegu_sheep.Add_child( rm_sheep )					// we become a child so that if the master vol is adjusted we'll react too

	p := cfg_data["default"]["queue_type"]				// lives in default b/c used by fq-mgr too
	if p != nil {
		if *p == "endpoint" {
			queue_gen_type = REQ_GEN_EPQMAP
		} else {
			queue_gen_type = REQ_GEN_QMAP
		}
	}

	cdp := cfg_data["resmgr"]["chkpt_dir"] 
	if cdp == nil {
		ckptd = "/var/lib/tegu/resmgr"							// default directory and prefix
	} else {
		ckptd = *cdp + "/resmgr"							// add prefix to directory in config
	}

	p = cfg_data["resmgr"]["verbose"]
	if p != nil {
		rm_sheep.Set_level(  uint( clike.Atoi( *p ) ) )
	}

	inv = Mk_inventory( )
	inv.chkpt = chkpt.Mk_chkpt( ckptd, 10, 90 )

	last_qcheck = time.Now().Unix()
	tklr.Add_spot( 2, my_chan, REQ_PUSH, nil, ipc.FOREVER )		// push reservations to skoogi just before they go live
	tklr.Add_spot( 1, my_chan, REQ_SETQUEUES, nil, ipc.FOREVER )	// drives us to see if queues need to be adjusted
	tklr.Add_spot( 180, my_chan, REQ_CHKPT, nil, ipc.FOREVER )		// tickle spot to drive us every 180 seconds to checkpoint

	rm_sheep.Baa( 3, "res_mgr is running  %x", my_chan )
	for {
		msg = <- my_chan					// wait for next message
		
		rm_sheep.Baa( 3, "processing message: %d", msg.Msg_type )
		switch msg.Msg_type {
			case REQ_NOOP:			// just ignore

			case REQ_ADD:
				p := msg.Req_data.( *gizmos.Pledge )	
				msg.State = inv.Add_res( p )
				msg.Response_data = nil

			case REQ_CHKPT:
				rm_sheep.Baa( 3, "invoking checkpoint" )
				inv.write_chkpt( )

			case REQ_DEL:											// user initiated delete -- requires cookie
				data := msg.Req_data.( []*string )					// assume pointers to name and cookie
				if *data[0] == "all" {
					inv.Del_all_res( data[1] )
					msg.State = nil
				} else {
					msg.State = inv.Del_res( data[0], data[1] )
				}

				msg.Response_data = nil

			case REQ_GET:											// user initiated get -- requires cookie
				data := msg.Req_data.( []*string )					// assume pointers to name and cookie
				msg.Response_data, msg.State = inv.Get_res( data[0], data[1] )

			case REQ_LIST:											// list reservations	(for a client)
				msg.Response_data, msg.State = inv.res2json( )

			case REQ_LOAD:								// load from a checkpoint file
				data := msg.Req_data.( *string )		// assume pointers to name and cookie
				msg.State = inv.load_chkpt( data )
				msg.Response_data = nil
				rm_sheep.Baa( 1, "checkpoint file loaded" )
	
			case REQ_PAUSE:
				msg.State = nil							// right now this cannot fail in ways we know about 
				msg.Response_data = ""
				inv.pause_on()
				rm_sheep.Baa( 1, "pausing..." )

			case REQ_RESUME:
				msg.State = nil							// right now this cannot fail in ways we know about 
				msg.Response_data = ""
				inv.pause_off()

			case REQ_SETQUEUES:							// driven about every second to reset the queues if a reservation state has changed
				now := time.Now().Unix()
				if now > last_qcheck  &&  inv.any_concluded( now - last_qcheck ) || inv.any_commencing( now - last_qcheck, 0 ) {
					rm_sheep.Baa( 1, "reservation state change detected, requesting queue map from net-mgr" )
					tmsg := ipc.Mk_chmsg( )
					tmsg.Send_req( nw_ch, my_chan, queue_gen_type, time.Now().Unix(), nil )		// get a queue map; when it arrives we'll push to fqmgr
				}
				last_qcheck = now

			case REQ_PUSH:								// driven every few seconds to push new reservations
				inv.push_reservations( my_chan )

			case REQ_PLEDGE_LIST:						// generate a list of pledges that are related to the given VM
				msg.Response_data, msg.State = inv.pledge_list(  msg.Req_data.( *string ) ) 

			// CAUTION: the requests below come back as asynch responses rather than as initial message
			case REQ_IE_RESERVE:						// an IE reservation failed
				msg.Response_ch = nil					// immediately disable to prevent loop
				inv.failed_push( msg )			// suss out the pledge and mark it unpushed

			case REQ_GEN_QMAP:							// response caries the queue map that now should be sent to fq-mgr to drive a queue update
				fallthrough

			case REQ_GEN_EPQMAP:
				rm_sheep.Baa( 1, "received queue map from network manager" )
				msg.Response_ch = nil											// immediately disable to prevent loop
				fq_data := make( []interface{}, 1 )
				fq_data[FQ_QLIST] = msg.Response_data 
				tmsg := ipc.Mk_chmsg( )
				tmsg.Send_req( fq_ch, nil, REQ_SETQUEUES, fq_data, nil )		// send the queue list to fq manager to deal with
				
			case REQ_YANK_RES:										// yank a reservation from the inventory returning the pledge and allowing flow-mods to purge
				if msg.Response_ch != nil {
					msg.Response_data, msg.State = inv.yank_res( msg.Req_data.( *string ) )
				}

			default:
				rm_sheep.Baa( 0, "WRN: res_mgr: unknown message: %d", msg.Msg_type )
				msg.Response_data = nil
				msg.State = fmt.Errorf( "res_mgr: unknown message (%d)", msg.Msg_type )
		}

		rm_sheep.Baa( 3, "processing message complete: %d", msg.Msg_type )
		if msg.Response_ch != nil {			// if a response channel was provided
			msg.Response_ch <- msg			// send our result back to the requestor
		}
	}
}
