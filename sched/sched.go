package sched

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/StackExchange/scollector/opentsdb"
	"github.com/StackExchange/tsaf/conf"
	"github.com/StackExchange/tsaf/expr"
)

type Schedule struct {
	*conf.Conf
	sync.Mutex
	Freq   time.Duration
	Status map[AlertKey]*State
	cache  *opentsdb.Cache
}

func (s *Schedule) MarshalJSON() ([]byte, error) {
	t := struct {
		Alerts map[string]*conf.Alert
		Freq   time.Duration
		Status map[string]*State
	}{
		s.Conf.Alerts,
		s.Freq,
		make(map[string]*State),
	}
	for k, v := range s.Status {
		if v.Last().Status < ST_WARN {
			continue
		}
		t.Status[k.String()] = v
	}
	return json.Marshal(&t)
}

var DefaultSched = &Schedule{
	Freq: time.Minute * 5,
}

// Loads a configuration into the default schedule
func Load(c *conf.Conf) {
	DefaultSched.Load(c)
}

// Runs the default schedule.
func Run() error {
	return DefaultSched.Run()
}

func (s *Schedule) Load(c *conf.Conf) {
	s.Conf = c
	s.RestoreState()
}

// Restores notification and alert state from the file on disk.
func (s *Schedule) RestoreState() {
	s.Lock()
	defer s.Unlock()
	s.cache = opentsdb.NewCache(s.Conf.TsdbHost)
	// Clear all existing notifications
	for _, st := range s.Status {
		st.Acknowledge()
	}
	s.Status = make(map[AlertKey]*State)
	f, err := os.Open(s.StateFile)
	if err != nil {
		log.Println(err)
		return
	}
	dec := json.NewDecoder(f)
	for {
		var ak AlertKey
		var st State
		if err := dec.Decode(&ak); err == io.EOF {
			break
		} else if err != nil {
			log.Println(err)
			return
		}
		if err := dec.Decode(&st); err != nil {
			log.Println(err)
			return
		}
		for k, v := range st.Notifications {
			n, present := s.Notifications[k]
			if !present {
				log.Println("sched: notification not present during restore:", k)
				continue
			}
			a, present := s.Alerts[ak.Name]
			if !present {
				log.Println("sched: alert not present during restore:", ak.Name)
				continue
			}
			go s.AddNotification(&st, a, n, st.Group, v)
		}
		s.Status[ak] = &st
	}
}

func (s *Schedule) Save() {
	s.Lock()
	defer s.Unlock()
	f, err := os.Create(s.StateFile)
	if err != nil {
		log.Println(err)
		return
	}
	enc := json.NewEncoder(f)
	for k, v := range s.Status {
		enc.Encode(k)
		enc.Encode(v)
	}
	if err := f.Close(); err != nil {
		log.Println(err)
		return
	}
	log.Println("sched: wrote state to", s.StateFile)
}

func (s *Schedule) Run() error {
	go func() {
		for _ = range time.Tick(time.Second * 20) {
			s.Save()
		}
	}()
	for {
		wait := time.After(s.Freq)
		if s.Freq < time.Second {
			return fmt.Errorf("sched: frequency must be > 1 second")
		}
		if s.Conf == nil {
			return fmt.Errorf("sched: nil configuration")
		}
		start := time.Now()
		s.Check()
		fmt.Printf("run at %v took %v\n", start, time.Since(start))
		<-wait
	}
}

func (s *Schedule) Check() {
	s.Lock()
	defer s.Unlock()
	s.cache = opentsdb.NewCache(s.Conf.TsdbHost)
	for _, a := range s.Conf.Alerts {
		s.CheckAlert(a)
	}
}

func (s *Schedule) CheckAlert(a *conf.Alert) {
	ignore := s.CheckExpr(a, a.Crit, true, nil)
	s.CheckExpr(a, a.Warn, false, ignore)
}

func (s *Schedule) CheckExpr(a *conf.Alert, e *expr.Expr, isCrit bool, ignore []AlertKey) (alerts []AlertKey) {
	if e == nil {
		return
	}
	results, err := e.Execute(s.cache, nil)
	if err != nil {
		// todo: do something here?
		log.Println(err)
		return
	}
Loop:
	for _, r := range results {
		if a.Squelched(r.Group) {
			continue
		}
		ak := AlertKey{a.Name, r.Group.String()}
		for _, v := range ignore {
			if ak == v {
				continue Loop
			}
		}
		state := s.Status[ak]
		if state == nil {
			state = &State{
				Group:        r.Group,
				Computations: r.Computations,
			}
		}
		status := ST_WARN
		if r.Value.(expr.Number) == 0 {
			status = ST_NORM
		} else if isCrit {
			status = ST_CRIT
		}
		notify := state.Append(status)
		s.Status[ak] = state
		if status != ST_NORM {
			alerts = append(alerts, ak)
			state.Expr = e.String()
			var subject = new(bytes.Buffer)
			if err := a.ExecuteSubject(subject, r.Group, s.cache); err != nil {
				log.Println(err)
			}
			state.Subject = subject.String()
		}
		if notify {
			for _, n := range a.Notification {
				go s.Notify(state, a, n, r.Group)
			}
		}
	}
	return
}

func (s *Schedule) Notify(st *State, a *conf.Alert, n *conf.Notification, group opentsdb.TagSet) {
	if len(n.Email) > 0 {
		go s.Email(a, n, group)
	}
	if n.Post != nil {
		go s.Post(a, n, group)
	}
	if n.Get != nil {
		go s.Get(a, n, group)
	}
	if n.Print {
		go s.Print(a, n, group)
	}
	// Cannot depend on <-st.ack always returning if it is closed because n.Timeout could be == 0, so check the bit here.
	if n.Next == nil || st.Acknowledged {
		return
	}
	s.AddNotification(st, a, n, group, time.Now())
}

func (s *Schedule) AddNotification(st *State, a *conf.Alert, n *conf.Notification, group opentsdb.TagSet, started time.Time) {
	st.Lock()
	if st.Notifications == nil {
		st.Notifications = make(map[string]time.Time)
	}
	// Prevent duplicate notification chains on the same state.
	if _, present := st.Notifications[n.Name]; !present {
		st.Notifications[n.Name] = time.Now().UTC()
	}
	st.Unlock()
	select {
	case <-st.ack:
		// break
	case <-time.After(n.Timeout - time.Since(started)):
		go s.Notify(st, a, n.Next, group)
	}
	st.Lock()
	delete(st.Notifications, n.Name)
	st.Unlock()
}

type AlertKey struct {
	Name  string
	Group string
}

func (a AlertKey) String() string {
	return a.Name + a.Group
}

type State struct {
	sync.Mutex

	// Most recent event last.
	History       []Event
	Touched       time.Time
	Expr          string
	Group         opentsdb.TagSet
	Computations  expr.Computations
	Subject       string
	Acknowledged  bool
	Notifications map[string]time.Time

	ack chan interface{}
}

func (s *State) Acknowledge() {
	if s.Acknowledged {
		return
	}
	s.Acknowledged = true
	if s.ack != nil {
		close(s.ack)
	}
}

func (s *State) Touch() {
	s.Touched = time.Now().UTC()
}

// Appends status to the history if the status is different than the latest
// status. Returns true if the status was changed to ST_CRIT. If the status was
// already ST_CRIT, returns false.
func (s *State) Append(status Status) bool {
	s.Touch()
	if len(s.History) == 0 || s.Last().Status != status {
		s.History = append(s.History, Event{status, time.Now().UTC()})
		s.Acknowledged = status != ST_CRIT
		if !s.Acknowledged {
			s.ack = make(chan interface{})
		}
		return status == ST_CRIT
	}
	return false
}

func (s *State) Last() Event {
	return s.History[len(s.History)-1]
}

type Event struct {
	Status Status
	Time   time.Time // embedding this breaks JSON encoding
}

type Status int

const (
	ST_NORM Status = iota
	ST_WARN
	ST_CRIT
)

func (s Status) String() string {
	switch s {
	case ST_NORM:
		return "normal"
	case ST_WARN:
		return "warning"
	case ST_CRIT:
		return "critical"
	default:
		return "unknown"
	}
}
