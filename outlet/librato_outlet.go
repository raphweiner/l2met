package outlet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"l2met/bucket"
	"l2met/token"
	"l2met/utils"
	"net/http"
	"strconv"
	"time"
)

var libratoUrl = "https://metrics-api.librato.com/v1/metrics"

type LibratoAttributes struct {
	Min   int    `json:"display_min"`
	Units string `json:"display_units_long"`
}

type LibratoPayload struct {
	Name   string             `json:"name"`
	Time   int64              `json:"measure_time"`
	Source string             `json:"source,omitempty"`
	User   string             `json:",omitempty"`
	Pass   string             `json:",omitempty"`
	Attrs  *LibratoAttributes `json:"attributes,omitempty"`
	Count  int                `json:"count"`
	Sum    float64            `json:"sum"`
	Min    float64            `json:"min"`
	Max    float64            `json:"max"`
}

type LibratoRequest struct {
	Gauges []*LibratoPayload `json:"gauges"`
}

type LibratoOutlet struct {
	// The inbox is used to hold empty buckets that are
	// waiting to be processed. We buffer the chanel so
	// as not to slow down the fetch routine.
	Inbox chan *bucket.Bucket
	// The converter will take items from the inbox,
	// fill in the bucket with the vals, then convert the
	// bucket into a librato metric.
	Conversions chan *LibratoPayload
	// The converter will place the librato metrics into
	// the outbox for HTTP submission. We rely on the batch
	// routine to make sure that the collections of librato metrics
	// in the outbox are homogeneous with respect to their token.
	// This ensures that we route the metrics to the correct librato account.
	Outbox chan []*LibratoPayload
	// How many outlet routines should be running.
	NumOutlets int
	// How many accept routines should be running.
	NumConverters int
	Reader        Reader
	Retries       int
	User          string
	Pass          string
}

func NewLibratoOutlet(mi, mc, mo int) *LibratoOutlet {
	l := new(LibratoOutlet)
	l.Inbox = make(chan *bucket.Bucket, mi)
	l.Conversions = make(chan *LibratoPayload, mc)
	l.Outbox = make(chan []*LibratoPayload, mo)
	return l
}

func (l *LibratoOutlet) Start() {
	go l.Reader.Start(l.Inbox)
	for i := 0; i < l.NumConverters; i++ {
		go l.convert()
	}
	go l.batch()
	for i := 0; i < l.NumOutlets; i++ {
		go l.outlet()
	}
	go l.report()
}

func (l *LibratoOutlet) report() {
	for _ = range time.Tick(time.Second * 2) {
		utils.MeasureI("librato-outlet.inbox", int64(len(l.Inbox)))
		utils.MeasureI("librato-outlet.conversions", int64(len(l.Conversions)))
		utils.MeasureI("librato-outlet.outbox", int64(len(l.Outbox)))
	}
}

func (l *LibratoOutlet) convert() {
	for bucket := range l.Inbox {
		if len(bucket.Vals) == 0 {
			fmt.Printf("at=bucket-no-vals bucket=%s\n", bucket.Id.Name)
			continue
		}
		//TODO(ryandotsmith): This is getting out of control.
		//We need a succinct way to building payloads.
		attrs := &LibratoAttributes{Min: 0, Units: bucket.Id.Units}
		l.Conversions <- &LibratoPayload{
			Name:   bucket.Id.Name,
			Source: bucket.Id.Source,
			Time:   ft(bucket.Id.Time),
			User:   bucket.Id.User,
			Pass:   bucket.Id.Pass,
			Attrs:  attrs,
			Count:  bucket.Count(),
			Sum:    bucket.Sum(),
			Min:    bucket.Min(),
			Max:    bucket.Max(),
		}
		fmt.Printf("measure.bucket.conversion.delay=%d\n", bucket.Id.Delay(time.Now()))
	}
}

func (l *LibratoOutlet) batch() {
	ticker := time.Tick(time.Millisecond * 200)
	batchMap := make(map[string][]*LibratoPayload)
	for {
		select {
		case <-ticker:
			for k, v := range batchMap {
				if len(v) > 0 {
					l.Outbox <- v
				}
				delete(batchMap, k)
			}
		case payload := <-l.Conversions:
			index := payload.User + ":" + payload.Pass
			_, present := batchMap[index]
			if !present {
				batchMap[index] = make([]*LibratoPayload, 1, 300)
				batchMap[index][0] = payload
			} else {
				batchMap[index] = append(batchMap[index], payload)
			}
			if len(batchMap[index]) == cap(batchMap[index]) {
				l.Outbox <- batchMap[index]
				delete(batchMap, index)
			}
		}
	}
}

func (l *LibratoOutlet) outlet() {
	for payloads := range l.Outbox {
		if len(payloads) < 1 {
			fmt.Printf("at=%q\n", "empty-metrics-error")
			continue
		}

		// all buckets in the payload are from the same token/account
		// we can get the user/pass from any entry in the payload
		sample := payloads[0]
		tok := new(token.Token)

		switch {
		// If a global user/token is provided, use the token for all metrics.
		// This enable a databaseless librato_outlet.
		case len(l.User) > 0 && len(l.Pass) > 0:
			tok.User = l.User
			tok.Pass = l.Pass
		// If user is l2met find credentials from postgres
		case sample.User == "l2met":
			//The token is constructed from the HTTP request
			//with user="l2met" and password="token.id"
			//TODO(ryandotsmith): Remove this path once all incoming
			//requests are using dbless-auth
			tok.Id = sample.Pass
			tok.Get()
		// you're using librato credentials
		default:
			tok.User = sample.User
			tok.Pass = sample.Pass
		}

		reqBody := new(LibratoRequest)
		reqBody.Gauges = payloads
		j, err := json.Marshal(reqBody)
		if err != nil {
			fmt.Printf("at=json-marshal-error error=%s\n", err)
			continue
		}

		l.postWithRetry(tok, bytes.NewBuffer(j))
	}
}

func (l *LibratoOutlet) postWithRetry(tok *token.Token, body *bytes.Buffer) error {
	for i := 0; i <= l.Retries; i++ {
		if err := l.post(tok, body); err != nil {
			fmt.Printf("measure.librato.error=1 msg=%s attempt=%d\n", err, i)
			if i == l.Retries {
				return err
			}
			continue
		}
		return nil
	}
	//Should not be possible.
	return errors.New("Unable to post.")
}

func (l *LibratoOutlet) post(tok *token.Token, body *bytes.Buffer) error {
	defer utils.MeasureT("librato-post", time.Now())
	req, err := http.NewRequest("POST", libratoUrl, body)
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")
	req.SetBasicAuth(tok.User, tok.Pass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var m string
		s, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			m = fmt.Sprintf("error=failed-request code=%d", resp.StatusCode)
		} else {
			m = fmt.Sprintf("error=failed-request code=%d resp=body=%s req-body=%s",
				resp.StatusCode, s, body)
		}
		return errors.New(m)
	}
	return nil
}

func ff(x float64) string {
	return strconv.FormatFloat(x, 'f', 5, 64)
}

func fi(x int) string {
	return strconv.FormatInt(int64(x), 10)
}

func ft(t time.Time) int64 {
	return t.Unix()
}
