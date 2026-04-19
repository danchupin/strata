package lifecycle

import (
	"encoding/xml"
	"errors"
	"strings"
)

type Configuration struct {
	XMLName xml.Name `xml:"LifecycleConfiguration"`
	Rules   []Rule   `xml:"Rule"`
}

type Rule struct {
	ID                             string                          `xml:"ID"`
	Status                         string                          `xml:"Status"`
	Filter                         *Filter                         `xml:"Filter"`
	Prefix                         string                          `xml:"Prefix"`
	Transition                     *Transition                     `xml:"Transition"`
	Expiration                     *Expiration                     `xml:"Expiration"`
	NoncurrentVersionTransition    *NoncurrentVersionTransition    `xml:"NoncurrentVersionTransition"`
	NoncurrentVersionExpiration    *NoncurrentVersionExpiration    `xml:"NoncurrentVersionExpiration"`
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload `xml:"AbortIncompleteMultipartUpload"`
}

type NoncurrentVersionTransition struct {
	NoncurrentDays int    `xml:"NoncurrentDays"`
	StorageClass   string `xml:"StorageClass"`
}

type NoncurrentVersionExpiration struct {
	NoncurrentDays int `xml:"NoncurrentDays"`
}

type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation int `xml:"DaysAfterInitiation"`
}

func (r *Rule) HasNoncurrentActions() bool {
	return r.NoncurrentVersionTransition != nil || r.NoncurrentVersionExpiration != nil
}

type Filter struct {
	Prefix string `xml:"Prefix"`
}

type Transition struct {
	Days         int    `xml:"Days"`
	StorageClass string `xml:"StorageClass"`
}

type Expiration struct {
	Days                      int  `xml:"Days"`
	ExpiredObjectDeleteMarker bool `xml:"ExpiredObjectDeleteMarker"`
}

func Parse(blob []byte) (*Configuration, error) {
	if len(blob) == 0 {
		return nil, errors.New("empty lifecycle blob")
	}
	var cfg Configuration
	if err := xml.Unmarshal(blob, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (r *Rule) IsEnabled() bool {
	return strings.EqualFold(r.Status, "Enabled")
}

func (r *Rule) PrefixMatch() string {
	if r.Filter != nil && r.Filter.Prefix != "" {
		return r.Filter.Prefix
	}
	return r.Prefix
}
