package sync

type Once struct {
	epoch uint32
	m     Mutex
}

func (o *Once) Do(f func()) {
	o.m.Lock()
	defer o.m.Unlock()
	ep := poolEpoch()
	if o.epoch == 1 || o.epoch == ep {
		return
	}
	o.epoch = ep
	f()
}
