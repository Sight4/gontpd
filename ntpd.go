package gontpd

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/beevik/ntp"
)

var errNoMedian = errors.New("no median found")

type NTPd struct {
	template []byte

	cfg *Config

	peerList  []*peer
	stat      *ntpStat
	dropTable *dropTable

	sleep time.Duration
	delay time.Duration
	disp  time.Duration
}

func New(cfg *Config) (d *NTPd) {

	if cfg.MinPoll < minPoll {
		cfg.MinPoll = minPoll
	}
	if cfg.MaxPoll > maxPoll {
		cfg.MaxPoll = maxPoll
	}

	if cfg.RateSize < 0 {
		cfg.RateSize = 0
	}

	dt, err := newDropTable(cfg.DropCIDR)
	if err != nil {
		return
	}

	d = &NTPd{cfg: cfg,
		template:  newTemplate(),
		dropTable: dt,
	}
	if cfg.Metric != "" {
		d.stat = newNTPStat(cfg.Metric)
	}
	return d
}

func (d *NTPd) Run() (err error) {

	err = d.init()
	if err != nil {
		return
	}

	d.poll()
	median := d.find()
	if median == nil {
		err = errNoMedian
		return
	}
	err = syncClock(median.resp.ClockOffset, 0,
		d.cfg.ForceUpdate)
	if err != nil {
		log.Println("sync err:", err, " offset:", median.resp.ClockOffset)
		return
	}
	d.setTemplate(median)
	d.updateState(median)

	go d.listen()

	for {
		time.Sleep(d.sleep)
		d.poll()
		median = d.find()
		if median == nil {
			log.Println(errNoMedian)
			d.sleep = time.Second * 10
			continue
		}

		err = syncClock(median.resp.ClockOffset,
			uint8(median.resp.Leap), d.cfg.ForceUpdate)
		if err != nil {
			return
		}

		d.setTemplate(median)
		d.updateState(median)

		if absDuration(median.resp.ClockOffset) < time.Millisecond*20 {
			poll := median.peer.trustLevel
			if poll > d.cfg.MaxPoll {
				poll = d.cfg.MaxPoll
			}
			if poll < d.cfg.MinPoll {
				poll = d.cfg.MinPoll
			}

			for _, p := range d.peerList {
				if p.good && p.trustLevel < d.cfg.MaxPoll {
					p.trustLevel += 1
				}
			}

			d.sleep = pollTable[poll-minPoll]
		} else {
			d.sleep = pollTable[0]
			for i := 0; i < len(d.peerList); i++ {
				d.peerList[i].trustLevel = 1
			}
		}
		if d.stat != nil {
			d.stat.pollGauge.Set(d.sleep.Seconds())
		}
	}
}

func (d *NTPd) updateState(op *offsetPeer) {
	if d.stat != nil {
		d.stat.delayGauge.Set(d.delay.Seconds())
		d.stat.offsetGauge.Set(op.resp.ClockOffset.Seconds())
		d.stat.dispGauge.Set(d.disp.Seconds())
	}
}

func (d *NTPd) init() (err error) {
	pool := map[string][]net.IP{}
	for _, addr := range d.cfg.PeerList {
		ips, err := net.LookupIP(addr)
		if err != nil {
			log.Print(err)
			continue
		}
		pool[addr] = ips
	}

	for origin, ips := range pool {
		for _, ip := range ips {
			p := newPeer(origin, ip)
			if p == nil {
				log.Printf("peer:%s->%s init failed", origin, ip.String())
			}
			d.peerList = append(d.peerList, p)
		}
	}

	if len(d.peerList) == 0 {
		err = fmt.Errorf("no available peer, tried: %v", d.cfg.PeerList)
	}

	d.sleep = pollTable[0]
	log.Printf("init with %d peers", len(d.peerList))

	return
}

func (d *NTPd) poll() {
	var wg sync.WaitGroup
	for _, p := range d.peerList {
		if p.enable {
			wg.Add(1)
			go p.update(&wg, d.cfg.MaxStd)
		}
	}
	wg.Wait()

	goodCount := 0
	for _, p := range d.peerList {
		if p.good {
			goodCount += 1
		}
	}
	if goodCount < 3 {
		log.Print("not enough good peers, but continue")
	}
}

type offsetPeer struct {
	peer *peer
	resp *ntp.Response
}

func (d *NTPd) find() (op *offsetPeer) {

	tmp := []*offsetPeer{}
	for _, p := range d.peerList {
		if !p.good {
			continue
		}

		for _, resp := range p.reply {
			if resp.Stratum >= invalidStratum {
				continue
			}
			tmp = append(tmp, &offsetPeer{p, resp})
		}
	}

	if len(tmp) == 0 {
		return
	}
	sort.Sort(byOffset(tmp))
	if debug {
		for _, p := range tmp {
			fmt.Printf("%s:%s,", p.peer.addr, p.resp.ClockOffset)
		}
		fmt.Print("\n")
	}

	if len(tmp) < goodFilter {
		return
	}

	op = tmp[len(tmp)/2]
	return
}

type byOffset []*offsetPeer

func (b byOffset) Len() int {
	return len(b)
}

func (b byOffset) Less(i, j int) bool {
	return b[i].resp.ClockOffset < b[j].resp.ClockOffset
}

func (b byOffset) Swap(i, j int) {
	b[i], b[j] = b[j], b[i]
}
