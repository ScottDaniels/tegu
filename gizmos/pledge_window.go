// vi: sw=4 ts=4:

/*

	Mnemonic:	pledge_window
	Abstract:	Struct that manages a window of time for any pledge type and the
				functions which make testing times against the window easier.
				Pledge times are managed at the second level; no need for more
				precision for this. This structure is local to gizmos so nothing
				should be visible to the outside world.

	Date:		20 May 2015
	Author:		E. Scott Daniels

	Mods:		
*/

package gizmos

import (
	"fmt"
	"time"
)

type pledge_window struct {
	commence	int64
	expiry		int64
}

/*
	Make a new pledge_window. If the commence time is earlier than now, it is adjusted
	to be now.  If the expry time is before the adjusted commence time, then a nil
	pointer and error are returned.
*/
func mk_pledge_window( commence int64, expiry int64 ) ( pw *pledge_window, err error ) {
	now := time.Now().Unix()
	err = nil
	pw = nil

	if commence < now {
		commence = now
	}

	if expiry < commence {
		err = fmt.Errorf( "bad expiry submitted, already expired: now=%d expiry=%d", now, expiry );
		obj_sheep.Baa( 2, "pledge: %s", err )
		return
	}

	pw = &pledge_window {
		commence: commence,
		expiry: expiry,
	}

	return
}

/*
	Adjust window. Returns a valid commence time (if earlier than now) or 0 if the
	time window is not valid.
func adjust_window( commence int64, conclude int64 ) ( adj_start int64, err error ) {

	now := time.Now().Unix()
	err = nil

	if commence < now {				// ajust forward to better play with windows on the paths
		adj_start = now
	} else {
		adj_start = commence
	}

	if conclude <= adj_start {						// bug #156 fix
		err = fmt.Errorf( "bad expiry submitted, already expired: now=%d expiry=%d", now, conclude );
		obj_sheep.Baa( 2, "pledge: %s", err )
		return
	}

	return
}
*/

func (p *pledge_window) clone( ) ( npw *pledge_window ) {
	if p == nil {
		return nil
	}

	npw = &pledge_window {
		expiry: p.expiry,
		commence: p.commence,
	}

	return
}

/*
	Return the state as a string and the amount of time in the 
	past (seconds) that the pledge expired, or the amount of 
	time in the future that the pledge will be active. Caption
	is a string such as "ago"  that can be used following the value
	if needed.
*/
func (p *pledge_window) state_str( ) ( state string, caption string, diff int64 ) {
	if p == nil {
		return "EXPIRED", "no window", 0
	}

	now := time.Now().Unix()

	if now >= p.expiry {
		state = "EXPIRED"
		caption = "ago"
	} else {
		if now < p.commence {
			state = "PENDING"
			diff = p.commence - now
			caption = "from now"
		} else {
			state = "ACTIVE"
			diff = p.expiry -  now
			caption = "remaining"
		}
	}

	return state, caption, diff
}

/*
	Extend the expiry time by n seconds. N may be negative and will not set the 
	expiry time earlier than now.
*/
func (p *pledge_window) extend_by( n int64 ) {
	if p == nil {
		return
	}

	p.expiry += n;

	if n < 0 {
		now := time.Now().Unix()
		if p.expiry < now {
			p.expiry = now
		}
	}
}

/*
	Set the expiry time to the timestamp passed in.
	It is valid to set the expiry time to a time before the current time. 
*/
func (p *pledge_window) set_expiry_to( new_time int64 ) {
	p.expiry = new_time;
}

/*
	Returns true if the pledge has expired (the current time is greather than
	the expiry time in the pledge).
*/
func (p *pledge_window) is_expired( ) ( bool ) {
	if p == nil {
		return true
	}

	return time.Now().Unix( ) >= p.expiry
}

/*
	Returns true if the pledge has not become active (the commence time is >= the current time).
*/
func (p *pledge_window) is_pending( ) ( bool ) {
	if p == nil {
		return false
	}
	return time.Now().Unix( ) < p.commence
}

/*
	Returns true if the pledge is currently active (the commence time is <= than the current time
	and the expiry time is > the current time.
*/
func (p *pledge_window) is_active( ) ( bool ) {
	if p == nil {
		return false
	}

	now := time.Now().Unix()
	return p.commence < now  && p.expiry > now
}

/*
	Returns true if pledge is active now, or will be active before elapsed seconds (window) have passed.
*/
func (p *pledge_window) is_active_soon( window int64 ) ( bool ) {
	if p == nil {
		return false
	}

	now := time.Now().Unix()
	return (p.commence >= now) && p.commence <= (now + window)
}

func (p *pledge_window) get_values( ) ( commence int64, expiry int64 ) {
	if p == nil {
		return 0, 0
	}

	return p.commence, p.expiry
}

/*
	Returns true if pledge concluded between (now - window) and now-1.
	If pledge_window is nil, then we return true.
*/
func (p *pledge_window) concluded_recently( window int64 ) ( bool ) {
	if p == nil {
		return true
	}

	now := time.Now().Unix()
	return (p.expiry < now)  && (p.expiry >= now - window)
}

/*
	Returns true if pledge started recently (between now and now - window seconds) and
	has not expired yet. If the pledge started within the window, but expired before
	the call to this function false is returned.
*/
func (p *pledge_window) commenced_recently( window int64 ) ( bool ) {
	if p == nil {
		return false
	}

	now := time.Now().Unix()
	return (p.commence >= (now - window)) && (p.commence <= now ) && (p.expiry > now)
}

/*
	Returns true if pledge expired long enough ago, based on the window timestamp 
	passed in,  that it can safely be discarded.  The window is the number of 
	seconds that the pledge must have been expired to be considered extinct.
*/
func (p *pledge_window) is_extinct( window int64 ) ( bool ) {
	if p == nil {
		return false
	}

	now := time.Now().Unix()
	return p.expiry <= now - window
}