package lifecycle

import "testing"

func TestParseFullConfig(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration>
  <Rule>
    <ID>transition-old-objects</ID>
    <Status>Enabled</Status>
    <Filter><Prefix>logs/</Prefix></Filter>
    <Transition>
      <Days>30</Days>
      <StorageClass>STANDARD_IA</StorageClass>
    </Transition>
    <Expiration>
      <Days>365</Days>
    </Expiration>
  </Rule>
  <Rule>
    <ID>noncurrent</ID>
    <Status>Enabled</Status>
    <Filter><Prefix></Prefix></Filter>
    <NoncurrentVersionTransition>
      <NoncurrentDays>14</NoncurrentDays>
      <StorageClass>GLACIER_IR</StorageClass>
    </NoncurrentVersionTransition>
    <NoncurrentVersionExpiration>
      <NoncurrentDays>90</NoncurrentDays>
    </NoncurrentVersionExpiration>
  </Rule>
  <Rule>
    <ID>kill-abandoned</ID>
    <Status>Enabled</Status>
    <AbortIncompleteMultipartUpload>
      <DaysAfterInitiation>7</DaysAfterInitiation>
    </AbortIncompleteMultipartUpload>
  </Rule>
</LifecycleConfiguration>`)

	cfg, err := Parse(blob)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(cfg.Rules) != 3 {
		t.Fatalf("rules: got %d want 3", len(cfg.Rules))
	}

	r0 := cfg.Rules[0]
	if !r0.IsEnabled() || r0.PrefixMatch() != "logs/" {
		t.Errorf("rule0 filter: %+v", r0)
	}
	if r0.Transition == nil || r0.Transition.Days != 30 || r0.Transition.StorageClass != "STANDARD_IA" {
		t.Errorf("rule0 transition: %+v", r0.Transition)
	}
	if r0.Expiration == nil || r0.Expiration.Days != 365 {
		t.Errorf("rule0 expiration: %+v", r0.Expiration)
	}

	r1 := cfg.Rules[1]
	if !r1.HasNoncurrentActions() {
		t.Errorf("rule1 should have noncurrent actions")
	}
	if r1.NoncurrentVersionTransition.NoncurrentDays != 14 {
		t.Errorf("rule1 noncurrent days: %+v", r1.NoncurrentVersionTransition)
	}
	if r1.NoncurrentVersionExpiration.NoncurrentDays != 90 {
		t.Errorf("rule1 noncurrent expiration: %+v", r1.NoncurrentVersionExpiration)
	}

	r2 := cfg.Rules[2]
	if r2.AbortIncompleteMultipartUpload == nil || r2.AbortIncompleteMultipartUpload.DaysAfterInitiation != 7 {
		t.Errorf("rule2 abort: %+v", r2.AbortIncompleteMultipartUpload)
	}
}

func TestParseLegacyPrefixField(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration><Rule><ID>legacy</ID><Status>Enabled</Status><Prefix>old/</Prefix><Expiration><Days>10</Days></Expiration></Rule></LifecycleConfiguration>`)
	cfg, err := Parse(blob)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Rules[0].PrefixMatch() != "old/" {
		t.Errorf("legacy prefix not matched: %+v", cfg.Rules[0])
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse(nil); err == nil {
		t.Error("expected error on empty blob")
	}
}

func TestDisabledRuleIsNotEnabled(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration><Rule><ID>off</ID><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`)
	cfg, _ := Parse(blob)
	if cfg.Rules[0].IsEnabled() {
		t.Error("Disabled rule should not be enabled")
	}
}

func TestValidateRejectsZeroDaysTransition(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Transition><Days>0</Days><StorageClass>STANDARD_IA</StorageClass></Transition></Rule></LifecycleConfiguration>`)
	cfg, err := Parse(blob)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if vErr := cfg.Validate(); vErr == nil {
		t.Fatal("expected Validate to reject Days=0 on Transition")
	}
}

func TestValidateRejectsZeroDaysExpiration(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Expiration><Days>0</Days></Expiration></Rule></LifecycleConfiguration>`)
	cfg, _ := Parse(blob)
	if vErr := cfg.Validate(); vErr == nil {
		t.Fatal("expected Validate to reject Days=0 on Expiration")
	}
}

func TestValidateAllowsExpiredObjectDeleteMarkerWithoutDays(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Expiration><ExpiredObjectDeleteMarker>true</ExpiredObjectDeleteMarker></Expiration></Rule></LifecycleConfiguration>`)
	cfg, _ := Parse(blob)
	if vErr := cfg.Validate(); vErr != nil {
		t.Fatalf("Validate should accept ExpiredObjectDeleteMarker without Days: %v", vErr)
	}
}

func TestValidateAcceptsPositiveDays(t *testing.T) {
	blob := []byte(`<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Transition><Days>30</Days><StorageClass>STANDARD_IA</StorageClass></Transition><Expiration><Days>365</Days></Expiration></Rule></LifecycleConfiguration>`)
	cfg, _ := Parse(blob)
	if vErr := cfg.Validate(); vErr != nil {
		t.Fatalf("Validate should accept positive Days: %v", vErr)
	}
}
