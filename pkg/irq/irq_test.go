// Copyright The NRI Plugins Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package irq

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/containers/nri-plugins/pkg/utils/cpuset"
)

const sampleInterrupts = `           CPU0       CPU1       CPU2       CPU3
  1:          0          0     601604          0  IR-IO-APIC    1-edge      i8042
  9:     236650          0          0          0  IR-IO-APIC    9-fasteoi   acpi
 16:          0          0         32          0  IR-IO-APIC   16-fasteoi   i801_smbus, processor_thermal_device_pci
NMI:          0          0          0          0  Non-maskable interrupts
LOC:     123456     123456     123456     123456  Local timer interrupts
`

func TestInterruptsAndMatch(t *testing.T) {
	dir := t.TempDir()
	SetProcRoot(dir)
	if err := os.Mkdir(filepath.Join(dir, "proc"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proc", "interrupts"), []byte(sampleInterrupts), 0644); err != nil {
		t.Fatal(err)
	}

	irqs, err := Interrupts()
	if err != nil {
		t.Fatalf("Interrupts() failed: %v", err)
	}
	if len(irqs) != 3 {
		t.Fatalf("expected 3 numbered interrupts, got %d: %v", len(irqs), irqs)
	}

	byNum := map[int]*Irq{}
	for _, irq := range irqs {
		byNum[irq.Num()] = irq
	}

	if byNum[1] == nil || byNum[9] == nil || byNum[16] == nil {
		t.Fatalf("missing expected interrupts, got %v", irqs)
	}
	if got := byNum[1].Description(); got != "IR-IO-APIC 1-edge i8042" {
		t.Errorf("irq 1 descriptor = %q", got)
	}

	cases := []struct {
		num   int
		claim string
		want  bool
	}{
		{1, "1", true},
		{1, "*i8042*", true},
		{1, "*acpi*", false},
		{9, "*acpi*", true},
		{16, "*processor_thermal_device_pci*", true},
		{16, "9", false},
		{1, "", false},
	}
	for _, c := range cases {
		if got := byNum[c.num].Match(c.claim); got != c.want {
			t.Errorf("irq %d Match(%q) = %v, want %v", c.num, c.claim, got, c.want)
		}
	}
}

func TestAffinityReadWrite(t *testing.T) {
	dir := t.TempDir()
	SetProcRoot(dir)
	irqDir := filepath.Join(dir, "proc", "irq", "42")
	if err := os.MkdirAll(irqDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(irqDir, "smp_affinity_list"), []byte("0-3\n"), 0644); err != nil {
		t.Fatal(err)
	}

	irq := &Irq{num: 42}
	cpus, err := irq.AffinityCpus()
	if err != nil {
		t.Fatalf("AffinityCpus() failed: %v", err)
	}
	if !cpus.Equals(cpuset.MustParse("0-3")) {
		t.Errorf("AffinityCpus() = %q, want 0-3", cpus)
	}

	if err := irq.SetAffinityCpus(cpuset.MustParse("1,3")); err != nil {
		t.Fatalf("SetAffinityCpus() failed: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(irqDir, "smp_affinity_list"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "1,3" {
		t.Errorf("written affinity = %q, want 1,3", string(data))
	}

	if err := irq.SetAffinityCpus(cpuset.New()); err == nil {
		t.Errorf("SetAffinityCpus(empty) should fail")
	}
}
