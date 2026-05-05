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

// Validate checks the lifecycle configuration for AWS-compatible
// constraints that the worker silently skips. Returns nil when the config
// is acceptable; otherwise an error suitable for surfacing as
// `InvalidArgument` on PutBucketLifecycleConfiguration.
//
// AWS rejects Days=0 (and negative values) with `InvalidArgument` for both
// `Transition` and `Expiration`. Without this check Strata used to accept
// the bad config and the worker would skip the rule on every tick — the
// operator saw no error but transitions never fired.
func (c *Configuration) Validate() error {
	for i := range c.Rules {
		r := &c.Rules[i]
		if r.Transition != nil && r.Transition.StorageClass != "" {
			if r.Transition.Days < 1 {
				return errors.New("'Days' for Transition action must be a positive integer")
			}
		}
		if r.Expiration != nil {
			// Expiration may be ExpiredObjectDeleteMarker-only with Days=0;
			// only the Days-positive case requires the gate.
			if !r.Expiration.ExpiredObjectDeleteMarker && r.Expiration.Days < 1 {
				return errors.New("'Days' for Expiration action must be a positive integer")
			}
		}
		if r.NoncurrentVersionTransition != nil && r.NoncurrentVersionTransition.StorageClass != "" {
			if r.NoncurrentVersionTransition.NoncurrentDays < 1 {
				return errors.New("'NoncurrentDays' for NoncurrentVersionTransition must be a positive integer")
			}
		}
		if r.NoncurrentVersionExpiration != nil {
			if r.NoncurrentVersionExpiration.NoncurrentDays < 1 {
				return errors.New("'NoncurrentDays' for NoncurrentVersionExpiration must be a positive integer")
			}
		}
		if r.AbortIncompleteMultipartUpload != nil {
			if r.AbortIncompleteMultipartUpload.DaysAfterInitiation < 1 {
				return errors.New("'DaysAfterInitiation' for AbortIncompleteMultipartUpload must be a positive integer")
			}
		}
	}
	return nil
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
