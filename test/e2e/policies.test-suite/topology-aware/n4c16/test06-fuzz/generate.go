package main

import (
	"flag"
	"fmt"
	"runtime"
	"sort"
	"strings"

	m "github.com/askervin/gofmbt/gofmbt"
)

type PodResources struct {
	cpu  int // total CPU allocated for all containers in the pod
	rcpu int // total reserved CPU allocated for all containers in the pod
	mem  int // total memory allocated for all containers in the pod
}

type TestState struct {
	cpu    int                      // free CPU on node for non-reserved pods
	rcpu   int                      // free CPU on node for reserved pods
	mem    int                      // free memory on node
	podRes map[string]*PodResources // map running pod name to resources allocated to it
}

func (s *TestState) String() string {
	pr := []string{}
	pods := make([]string, 0, len(s.podRes))
	for pod := range s.podRes {
		pods = append(pods, pod)
	}
	sort.Strings(pods)
	for _, pod := range pods {
		res := s.podRes[pod]
		switch {
		case res.rcpu == 0:
			pr = append(pr, fmt.Sprintf("%s:%dmCPU/%dM", pod, res.cpu, res.mem))
		case res.cpu == 0:
			pr = append(pr, fmt.Sprintf("%s:%dmRCPU/%dM", pod, res.rcpu, res.mem))
		default:
			pr = append(pr, fmt.Sprintf("%s:%dmCPU/%dmRCPU/%dM", pod, res.cpu, res.rcpu, res.mem))
		}
	}
	return fmt.Sprintf("[free:%dmCPU/%dmRCPU/%dM pods:[%s]]", s.cpu, s.rcpu, s.mem, strings.Join(pr, " "))
}

func createPod(pod string, cpu, rcpu, mem int) m.StateChange {
	return func(current m.State) m.State {
		s := current.(*TestState)
		if s.cpu < cpu || s.rcpu < rcpu || s.mem < mem {
			// refuse from state change if not enough resources
			return nil
		}
		if _, ok := s.podRes[pod]; ok {
			// refuse to create pod if it is already running
			return nil
		}
		newPodRes := make(map[string]*PodResources)
		for k, v := range s.podRes {
			newPodRes[k] = v
		}
		newPodRes[pod] = &PodResources{cpu, rcpu, mem}
		return &TestState{
			cpu:    s.cpu - cpu,
			rcpu:   s.rcpu - rcpu,
			mem:    s.mem - mem,
			podRes: newPodRes,
		}
	}
}

func deletePod(pod string) m.StateChange {
	return func(current m.State) m.State {
		s := current.(*TestState)
		res, ok := s.podRes[pod]
		if !ok {
			// refuse to delete pod if it is not running
			return nil
		}
		newPodRes := make(map[string]*PodResources)
		for k, v := range s.podRes {
			if k != pod {
				newPodRes[k] = v
			}
		}
		return &TestState{
			cpu:    s.cpu + res.cpu,
			rcpu:   s.rcpu + res.rcpu,
			mem:    s.mem + res.mem,
			podRes: newPodRes,
		}
	}
}

func newModel() *m.Model {
	podNames := []string{"gu0", "gu1", "gu2", "gu3", "gu4", "bu0", "bu1", "be0", "be1"}
	rPodNames := []string{"rbe0", "rgu0", "rbu0"}

	model := m.NewModel()

	model.From(func(current m.State) []*m.Transition {
		s := current.(*TestState)
		return m.When(true,
			m.OnAction("NAME=be0 CONTCOUNT=1 CPU=0 MEM=0 create besteffort").Do(createPod("be0", 1*0, 0, 1*0)),
			m.OnAction("NAME=be1 CONTCOUNT=3 CPU=0 MEM=0 create besteffort").Do(createPod("be1", 3*0, 0, 3*0)),
			m.OnAction("NAME=rbe0 CONTCOUNT=2 CPU=0 MEM=0 namespace=kube-system create besteffort").Do(createPod("rbe0", 0, 2*0, 2*0)),
			m.When(s.mem > 0,
				m.When(s.cpu >= 200,
					m.OnAction("NAME=gu0 CONTCOUNT=1 CPU=200m MEM=1500M create guaranteed").Do(createPod("gu0", 1*200, 0, 1*1500)),
					m.OnAction("NAME=gu1 CONTCOUNT=2 CPU=1000m MEM=500M create guaranteed").Do(createPod("gu1", 2*1000, 0, 2*500)),
					m.OnAction("NAME=gu2 CONTCOUNT=2 CPU=1200m MEM=4500M create guaranteed").Do(createPod("gu2", 2*1200, 0, 2*4500)),
					m.OnAction("NAME=gu3 CONTCOUNT=3 CPU=2000m MEM=500M create guaranteed").Do(createPod("gu3", 3*2000, 0, 3*500)),
					m.OnAction("NAME=gu4 CONTCOUNT=1 CPU=4200m MEM=100M create guaranteed").Do(createPod("gu4", 1*4200, 0, 1*100)),
					m.OnAction("NAME=bu0 CONTCOUNT=1 CPU=1200m MEM=50M CPUREQ=900m MEMREQ=49M CPULIM=1200m MEMLIM=50M create burstable").Do(createPod("bu0", 1*1200, 0, 1*50)),
					m.OnAction("NAME=bu1 CONTCOUNT=2 CPU=1900m MEM=300M CPUREQ=1800m MEMREQ=299M CPULIM=1900m MEMLIM=300M create burstable").Do(createPod("bu1", 2*1900, 0, 2*300)),
				),
				m.When(s.rcpu > 99,
					m.OnAction("NAME=rgu0 CONTCOUNT=2 CPU=100m MEM=1000M namespace=kube-system create guaranteed").Do(createPod("rgu0", 0, 2*100, 2*1000)),
					m.OnAction("NAME=rbu0 CONTCOUNT=1 CPU=100m MEM=100M CPUREQ=99m MEMREQ=99M CPULIM=100m MEMLIM=100M namespace=kube-system create burstable").Do(createPod("rbu0", 0, 1*100, 1*100)),
				),
			),
		)
	})

	model.From(func(current m.State) []*m.Transition {
		s := current.(*TestState)
		ts := []*m.Transition{}
		for _, pod := range podNames {
			if _, ok := s.podRes[pod]; ok {
				ts = append(ts, m.OnAction("NAME=%s vm-command 'kubectl delete pod %s --now'", pod, pod).Do(deletePod(pod))...)
			}
		}
		for _, pod := range rPodNames {
			if _, ok := s.podRes[pod]; ok {
				ts = append(ts, m.OnAction("NAME=%s vm-command 'kubectl delete pod --namespace kube-system %s --now'", pod, pod).Do(deletePod(pod))...)
			}
		}
		return ts
	})

	return model
}

var (
	maxMem         int
	maxCpu         int
	maxReservedCpu int
	maxTestSteps   int
	randomSeed     int64
	randomness     int
	searchDepth    int
)

func main() {
	flag.IntVar(&maxMem, "mem", 7500, "memory available for test pods")
	flag.IntVar(&maxCpu, "cpu", 15000, "non-reserved milli-CPU available for test pods")
	flag.IntVar(&maxReservedCpu, "reserved-cpu", 1000, "reserved milli-CPU availble for test pods")
	flag.IntVar(&maxTestSteps, "test-steps", 3000, "number of test steps")
	flag.Int64Var(&randomSeed, "random-seed", 0, "random seed for selecting best path")
	flag.IntVar(&randomness, "randomness", 0, "the greater the randomness, the larger the set of paths for choosing best path. 0 means no randomness, 5 picks any path that increases coverage.")
	flag.IntVar(&searchDepth, "search-depth", 4, "number of steps to look ahead when selecting best path")

	flag.Parse()

	_, generateGoFile, _, _ := runtime.Caller(0)

	model := newModel()
	coverer := m.NewCoverer()
	coverer.CoverActionCombinations(3)

	if randomSeed > 0 || randomness > 0 {
		coverer.SetBestPathRandom(randomSeed, randomness)
	}

	var state m.State

	state = &TestState{
		cpu:  maxCpu,
		rcpu: maxReservedCpu,
		mem:  maxMem,
	}
	fmt.Printf("echo === generated by: %s --mem=%d --cpu=%d --reserved-cpu=%d --test-steps=%d --random-seed=%d --randomness=%d --search-depth=%d\n",
		generateGoFile, maxMem, maxCpu, maxReservedCpu, maxTestSteps, randomSeed, randomness, searchDepth)
	testStep := 0
	for testStep < maxTestSteps {
		path, covStats := coverer.BestPath(model, state, searchDepth)
		if len(path) == 0 {
			fmt.Printf("# did not find anything to cover\n")
			break
		}
		for i := 0; i < covStats.MaxStep+1; i++ {
			testStep++
			step := path[i]
			fmt.Printf("\necho === step:%d coverage:%d state:%v\n", testStep, coverer.Coverage(), state)
			fmt.Println(step.Action())
			state = step.EndState()
			coverer.MarkCovered(step)
			coverer.UpdateCoverage()
			if testStep >= maxTestSteps {
				break
			}
		}
	}
}
