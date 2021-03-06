/*
 * memcache client
 */

package memcache

import (
	"errors"
	"math"
	"sync"
	"time"
	"log"
)

const (
	CMD_SET    = 0
	CMD_DELETE = 1
	CMD_INCR   = 2
	CMD_APPEND = 3
)

type Cmd struct {
	H *Host
	A int
	K string
	V interface{}
}

// Client of memcached
type Client struct {
	scheduler Scheduler
	N, W, R   int
	success   chan bool
	async     chan *Cmd
}

func (c *Client) Shutdown() {
	close(c.async)
}

func (c *Client) WaitForShutdown() {
	<-c.success
}

func ProcessCmd(cmd *Cmd) (ok bool, err error) {
	switch cmd.A {
	case CMD_SET:
		if item, ok1 := cmd.V.(*Item); ok1 {
			cmd.H.Set(cmd.K, item, false)
		} else {
			// TODO:log the typeassert error
			log.Println("async Set with wrong value type, it is not item")
			err = errors.New("async Set with wrong value type")
			ok = false
			return
		}
	case CMD_DELETE:
		cmd.H.Delete(cmd.K)
	case CMD_INCR:
		if v, ok1 := cmd.V.(int); ok1 {
			cmd.H.Incr(cmd.K, v)
		} else {
			// TODO:log the typeassert error
			log.Println("async Incr with wrong value type, it is not int")
			err = errors.New("async Incr with wrong type")
			ok = false
			return
		}
	case CMD_APPEND:
		if value, ok1 := cmd.V.([]byte); ok1 {
			cmd.H.Append(cmd.K, value)
		} else {
			// TODO:log the typeassert error
			log.Println("async Append with wrong value type, it is not []byte")
			err = errors.New("async Append with wrong type")
			ok = false
			return
		}
	default:
		// TODO:log the abnormal cmd type
		log.Println("async process cmd with wrong cmd type")
		err = errors.New("async process cmd with wrong cmd type")
		ok = false
		return
	}
	ok = true
	err = nil
	return
}

func GenerateCmd(host *Host, key string, value interface{}, action int) (cmd *Cmd) {
	cmd = new(Cmd)
	cmd.H = host
	cmd.K = key
	cmd.V = value
	cmd.A = action
	return
}

func (c *Client) TrySendCmd(cmd *Cmd) {
	select {
	case c.async <- cmd:
	default:
		// Make sure online request will never be blocked,
		// if channel is full, just drop more cmd
		//TODO: log if cmd send failed
	}
}

func (c *Client) AsyncModify() {
	var cmd *Cmd
	var ok bool
	for {
		select {
		case cmd, ok = (<-c.async):
			if ok {
				ProcessCmd(cmd)
			} else {
				c.success <- true
				// exit this goroutine
				return
			}
		default:
			//read cmd from channel failed
			continue
		}
	}
}

func NewClient(sch Scheduler) (c *Client) {
	c = new(Client)
	c.scheduler = sch
	c.N = 3
	c.W = 2
	c.R = 1
	c.success = make(chan bool, 1)
	c.async = make(chan *Cmd, 256)
	go c.AsyncModify()
	return c
}

func (c *Client) Get(key string) (r *Item, targets []string, err error) {
	hosts := c.scheduler.GetHostsByKey(key)
	cnt := 0
	for i, host := range hosts {
		st := time.Now()
		r, err = host.Get(key)
		if err != nil {
			c.scheduler.Feedback(host, key, -10, false)
		} else {
			cnt++
			if r != nil {
				t := float64(time.Now().Sub(st)) / 1e9
				c.scheduler.Feedback(host, key, -float64(math.Sqrt(t)*t), false)
				for j := 0; j < i; j++ {
					c.scheduler.Feedback(hosts[j], key, -1, false)
				}
				// got the right rval
				targets = []string{host.Addr}
				err = nil
				//return r, nil
				return
			}
		}
		if cnt >= c.R && i+1 >= c.N {
			// because hosts are sorted
			err = nil
			// because no item gotten
			break
		}
	}
	// here is a failure exit
	return
}

func (c *Client) getMulti(keys []string) (rs map[string]*Item, targets []string, err error) {
	need := len(keys)
	rs = make(map[string]*Item, need)
	hosts := c.scheduler.GetHostsByKey(keys[0])
	suc := 0
	for i, host := range hosts {
		st := time.Now()
		r, er := host.GetMulti(keys)
		if er != nil { // failed
			err = er
			c.scheduler.Feedback(host, keys[0], -10, false)
		} else {
			suc += 1
			targets = append(targets, host.Addr)
		}

		t := float64(time.Now().Sub(st)) / 1e9
		c.scheduler.Feedback(host, keys[0], -float64(math.Sqrt(t)*t), false)
		for k, v := range r {
			rs[k] = v
		}

		if len(rs) == need {
			break
		}
		if i+1 >= c.N && suc >= c.R {
			err = nil
			targets = []string{}
			break
		}

		new_keys := []string{}
		for _, k := range keys {
			if _, ok := rs[k]; !ok {
				new_keys = append(new_keys, k)
			}
		}
		keys = new_keys
		if len(keys) == 0 {
			break // repeated keys
		}
	}
	if len(rs) > 0 {
		err = nil
	}
	return
}

func (c *Client) GetMulti(keys []string) (rs map[string]*Item, targets []string, err error) {
	var lock sync.Mutex
	rs = make(map[string]*Item, len(keys))

	gs := c.scheduler.DivideKeysByBucket(keys)
	reply := make(chan bool, len(gs))
	for _, ks := range gs {
		if len(ks) > 0 {
			go func(keys []string) {
				r, t, e := c.getMulti(keys)
				if e != nil {
					err = e
				} else {
					for k, v := range r {
						lock.Lock()
						rs[k] = v
						targets = append(targets, t...)
						lock.Unlock()
					}
				}
				reply <- true
			}(ks)
		} else {
			reply <- true
		}
	}
	// wait for complete
	for _, _ = range gs {
		<-reply
	}
	return
}

func (c *Client) Set(key string, item *Item, noreply bool) (ok bool, targets []string, final_err error) {
	suc := 0
	got := false
	for i, host := range c.scheduler.GetHostsByKey(key) {
		if got {
			c.TrySendCmd(GenerateCmd(host, key, item, CMD_SET))
			break
		}
		if ok, err := host.Set(key, item, noreply); err == nil && ok {
			suc++
			targets = append(targets, host.Addr)
		} else {
			c.scheduler.Feedback(host, key, -2, false)
		}
		if suc >= c.W {
			if (i + 1) < c.N {
				got = true
				// first W all success, than async the N-W hosts
			} else {
				// if have try more or equal than c.N
				// no need to async
				break
			}
			// if it is the last host, async is no need
		}
	}
	if suc == 0 {
		ok = false
		final_err = errors.New("write failed")
		return
	}
	ok = (suc >= c.W)
	return
}

func (c *Client) Append(key string, value []byte) (ok bool, targets []string, final_err error) {
	suc := 0
	got := false
	for i, host := range c.scheduler.GetHostsByKey(key) {
		if got {
			c.TrySendCmd(GenerateCmd(host, key, value, CMD_APPEND))
			break
		}
		if ok, err := host.Append(key, value); err == nil && ok {
			suc++
			targets = append(targets, host.Addr)
		}
		if suc >= c.W {
			if (i + 1) < c.N {
				got = true
				// first W all success, than async the N-W hosts
			} else {
				// if have try more or equal than c.N
				// no need to async
				break
			}
			// if it is the last host, async is no need
		}
	}
	if suc == 0 {
		ok = false
		final_err = errors.New("write failed")
		return
	}
	ok = (suc >= c.W)
	return
}

func (c *Client) Incr(key string, value int) (result int, targets []string, err error) {
	//result := 0
	suc := 0
	//var err error
	got := false
	for i, host := range c.scheduler.GetHostsByKey(key) {
		if got {
			c.TrySendCmd(GenerateCmd(host, key, value, CMD_INCR))
			break
		}
		r, e := host.Incr(key, value)
		if e != nil {
			err = e
			continue
		}
		if r > 0 {
			suc++
			targets = append(targets, host.Addr)
		}
		if r > result {
			result = r
		}
		if suc >= c.W && (i+1) >= c.N {
			if (i + 1) < c.N {
				got = true
				// first W all success, than async the N-W hosts
			} else {
				// if have try more or equal than c.N
				// no need to async
				break
			}
		}
	}
	if result > 0 {
		err = nil
	}
	//return result, err // maximize
	return
}

func (c *Client) Delete(key string) (r bool, targets []string, err error) {
	suc := 0
	for _, host := range c.scheduler.GetHostsByKey(key) {
		ok, er := host.Delete(key)
		if er != nil {
			err = er
		} else if ok {
			suc++
			targets = append(targets, host.Addr)
		}
		if suc >= c.N {
			break
		}
	}
	if suc > 0 {
		err = nil
	}
	r = (suc >= c.W)
	//return suc >= c.W, err
	return
}

func (c *Client) Len() int {
	return 0
}
