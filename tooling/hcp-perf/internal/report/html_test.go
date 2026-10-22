// Copyright 2026 Microsoft Corporation
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

package report

import (
	"os/exec"
	"strings"
	"testing"
)

func TestFailureAndRecoveryHTML(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	const fixture = `
const cycle={name:'canary-steady-7',state:'failed',failureStage:'ack',
  ack:{started:'2026-01-01T00:00:00Z',durationNanoseconds:181350000},
  ready:{started:'2026-01-01T00:00:00Z',durationNanoseconds:181350000},
  cleanup:{started:'2026-01-01T00:00:00.182Z',completed:'2026-01-01T00:00:05.814Z',durationNanoseconds:5632000000}};
const group={passed:0,failed:0,pending:0,samples:[],ackSeconds:{count:0},readySeconds:{count:0},cleanupSeconds:{count:0}};
const fixtureData={progress:{baseline:group,steadyCanaries:{...group,passed:6,failed:1,samples:[cycle]},steady:{},deadlinesNanoseconds:{ack:180820000,ready:10000000000,cleanup:10000000000},observedAt:'2026-01-01T00:01:00Z'},limits:{},assessment:{status:{workload:'fail',capture:'complete'}}};
`
	const checks = `
const cells=id=>descend($(id),'td').map(n=>n.textContent),titles=descend($('timeline'),'title').map(n=>n.textContent);
assert.equal($('failure-panel').hidden,false);
assert.ok($('failure-summary').textContent.includes('Internal progress criterion missed'));
assert.equal(descend($('assessment-status'),'strong')[0].textContent,'Failed');
assert.ok(descend($('assessment-status'),'p')[0].textContent.includes('not a cluster-wide availability verdict'));
assert.ok(cells('canary-table').includes('Not measured: ACK stage failed'));
assert.ok(!titles.some(t=>t.includes('over budget')),'unobserved completion is not completed over-budget latency');
const bars=descend($('timeline'),'rect');
assert.deepEqual(bars.map(n=>n.attrs['data-stage']),['ack','cleanup'],'no duplicate Ready wait');
assert.equal(new Set(bars.map(n=>n.attrs.y)).size,2,'cleanup is in its own lane');
assert.equal(bars[0].attrs['data-status'],'failed');
assert.equal(bars[0].attrs.stroke,'#a32b24');
assert.equal(bars[0].attrs['stroke-dasharray'],'none','ended wait is not still pending');
assert.ok(descend($('timeline'),'path').length,'failed wait is hatched');
assert.equal(bars[1].attrs['data-status'],'completed');
assert.ok(titles.some(t=>t.includes('ACK not observed; client ended after 181.4ms')));
assert.ok(titles.some(t=>t.includes('Removed after scale-down 5.632 s')));
assert.ok(Math.abs(timelineRows[0].bars[0].end-timelineRows[0].bars[0].start-.18135)<1e-6,'failed bar ends at client duration, not snapshot');
assert.ok(cells('canary-table').includes('5.632'));
assert.ok(cells('failure-table').includes('Canary cleanup completed in 5.632 s'));
assert.ok($('worked-lead').textContent.includes('6 steady canaries passed'));
assert.ok($('worked-lead').textContent.includes('cleanup completed'));
assert.ok(descend($('failure-panel'),'p').some(n=>n.textContent.includes('does not prove recovery of every component')));
assert.ok(descend($('phase-table'),'th').some(n=>n.textContent==='Failed cycles (any stage)'));
assert.ok(!descend($('phase-table'),'th').some(n=>n.textContent==='Failed'));
`
	for _, tc := range []struct{ name, setup, checks string }{
		{"unclassified", "", checks + `
assert.ok(cells('canary-table').includes('Ended without ACK after 181.4ms (deadline 180.8ms); completed latency unknown'));
assert.ok(!cells('failure-table').some(t=>t.includes('Timed out')),'failure stage alone does not establish timeout');`},
		{"deadline-exceeded", `fixtureData.assessment.failures=[{command:cycle.name,stage:'ack',status:'deadline_exceeded',deadlineSeconds:.18082}];`, checks + `
assert.ok(cells('canary-table').includes('Timed out after 181.4ms (deadline 180.8ms); ACK completion not observed'));
assert.ok(cells('failure-table').includes('Client context deadline exceeded; server cause unknown'));`},
		{"assessed-timeout-and-recovery", `fixtureData.assessment.failures=[{command:cycle.name,stage:'ack',operation:'PUT deployments/scale',status:'client_timeout',deadlineSeconds:.18082,recovery:{status:'observed',elapsedSeconds:5.632,zeroReplicasObserved:true,noOwnedPodsObserved:true}}];`, `
assert.ok(descend($('canary-table'),'td').some(n=>n.textContent.includes('Timed out after 181.4ms')));
assert.ok(descend($('failure-table'),'td').some(n=>n.textContent.includes('zero replicas observed; no owned pods observed')));
assert.ok(descend($('timeline'),'title').some(n=>n.textContent.includes('zero replicas observed; no owned pods observed')));`},
		{"assessment-only", `fixtureData.progress.steadyCanaries.samples=[];fixtureData.assessment.failures=[{command:cycle.name,stage:'ack',status:'client_timeout',elapsedSeconds:.18135,deadlineSeconds:.18082,completionObserved:false,recovery:{status:'observed',elapsedSeconds:5.632,zeroReplicasObserved:true,noOwnedPodsObserved:true}}];`, `
assert.equal($('failure-panel').hidden,false);
assert.equal(descend($('failure-table'),'tbody')[0].children.length,1);
assert.ok(descend($('failure-table'),'td').some(n=>n.textContent==='181.4ms'));
assert.ok(descend($('failure-table'),'td').some(n=>n.textContent.includes('completion not observed')));
assert.ok($('worked-lead').textContent.includes('cleanup completed'));`},
		{"server-observation", `fixtureData.assessment.failures=[{command:cycle.name,stage:'ack',status:'client_timeout',completionObserved:false,serverObservation:{code:200,durationSeconds:.12,evidenceRefs:['audit'],unknownPrivateField:'DO-NOT-RENDER'}}];fixtureData.assessment.evidence=[{id:'audit',path:'audit-summary.json',sha256:'abc'}];`, `
assert.deepEqual(descend($('request-evidence'),'td').map(n=>n.textContent),['200','120.0ms','Not observed']);
assert.ok(descend($('request-evidence'),'p').some(n=>n.textContent.includes('does not by itself prove that this client received')));
assert.ok(descend($('request-evidence'),'p').some(n=>n.textContent==='audit-summary.json'));
assert.ok(!descend($('request-evidence'),'td').some(n=>n.textContent.includes('DO-NOT-RENDER')));
assert.ok(descend($('canary-table'),'td').some(n=>n.textContent.includes('ACK completion not observed')));`},
		{"missing-completions", `cycle.cleanup={durationNanoseconds:0};cycle.ack.durationNanoseconds=0;`, `
const cells=descend($('canary-table'),'td').map(n=>n.textContent);
assert.ok(cells.includes('unknown'),'zero default duration does not establish completed zero latency');
assert.ok(cells.some(t=>t.includes('after unknown')));
assert.ok(!descend($('timeline'),'rect').some(n=>n.attrs['data-status']==='completed'));
assert.ok(!descend($('failure-table'),'td').some(n=>n.textContent.includes('cleanup completed')));
assert.ok(!$('worked-lead').textContent.includes('cleanup completed'));`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.TrimSuffix(strings.TrimPrefix(htmlTail, "</script><script>"), "</script></body></html>")
			cmd := exec.Command(node)
			cmd.Stdin = strings.NewReader(mockDOM + fixture + tc.setup + "\ndocument.getElementById('report-data').textContent=JSON.stringify(fixtureData);\n" + script + tc.checks)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("failure/recovery rendering: %v\n%s", err, output)
			}
		})
	}
}

func TestCompletedLatencyDegradationHTML(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	const fixture = `
const stage={started:'2026-01-01T00:00:00Z',completed:'2026-01-01T00:00:00.501Z',durationNanoseconds:500000000,expectedDurationNanoseconds:180000000,deadlineNanoseconds:30000000000,expectationExceeded:true};
const cycle={name:'canary-steady-1',state:'completed',performance:'degraded',ack:stage,ready:stage,cleanup:stage};
const group={passed:0,failed:0,pending:0,samples:[],ackSeconds:{count:0},readySeconds:{count:0},cleanupSeconds:{count:0}};
const fixtureData={progress:{baseline:group,steadyCanaries:{...group,passed:2,expected:2,degraded:2,samples:[cycle,{...cycle,name:'canary-steady-2'}]},steady:{},expectedDurationsNanoseconds:{ack:9999999999,ready:9999999999,cleanup:9999999999},hardDeadlinesNanoseconds:{ack:99000000000,ready:99000000000,cleanup:99000000000},observedAt:'2026-01-01T00:01:00Z'},limits:{},assessment:{status:{workload:'pass',performance:'degraded',capture:'complete'},failures:[]}};
`
	const checks = `
assert.deepEqual(descend($('assessment-status'),'strong').map(n=>n.textContent),['Passed','Degraded','Complete']);
assert.ok($('worked-lead').textContent.includes('Completed, with latency degradation'));
assert.ok($('worked-lead').textContent.includes('2/2 steady canaries'));
assert.equal(failureRows.length,0,'soft overruns must not enter failure panel');
const cells=descend($('canary-table'),'td').map(n=>n.textContent),headers=descend($('canary-table'),'th').map(n=>n.textContent);
for(const title of ['ACK Expected s','ACK Hard stop s','ACK Completed elapsed s','ACK Status'])assert.ok(headers.includes(title));
for(const text of ['0.18','30','0.5','Completed late (degraded)'])assert.ok(cells.includes(text));
assert.ok(!cells.includes('9.99999999')&&!cells.includes('99'),'stage thresholds override summary');
for(const bar of descend($('timeline'),'rect')){assert.equal(bar.attrs['data-status'],'degraded');assert.equal(bar.attrs.fill,'#b77922');}
assert.ok(Math.abs(timelineRows[0].bars[0].end-timelineRows[0].bars[0].start-.5)<1e-6,'bar uses recorded actual duration, not soft expectation or rounded completion timestamp');
assert.ok(descend($('timeline'),'title').some(n=>n.textContent.includes('source: recorded durationNanoseconds')));
`
	for _, tc := range []struct{ name, setup, checks string }{
		{"completed late", "", checks},
		{"skipped", `fixtureData.progress.steadyCanaries.skipped=1;fixtureData.progress.steadyCanaries.expected=3;fixtureData.assessment.status.workload='incomplete';`, `assert.ok($('worked-lead').textContent.includes('2 completed steady canaries; 1 skipped observation slots, not workload failures'));assert.equal(failureRows.length,0);assert.ok(!$('worked-lead').textContent.includes('3/3'));`},
		{"legacy", `delete stage.expectedDurationNanoseconds;delete stage.deadlineNanoseconds;delete fixtureData.progress.hardDeadlinesNanoseconds;fixtureData.progress.deadlinesNanoseconds={ack:180000000,ready:180000000,cleanup:180000000};fixtureData.assessment.status.performance='unknown';`, `assert.ok(!descend($('timeline'),'rect').some(n=>n.attrs['data-status']==='degraded'));assert.equal(failureRows.length,0);`},
		{"legacy timeout", `delete stage.expectedDurationNanoseconds;delete stage.deadlineNanoseconds;delete stage.completed;cycle.state='failed';cycle.failureStage='ack';fixtureData.progress.steadyCanaries.samples=[cycle];fixtureData.assessment.status.workload='fail';fixtureData.assessment.failures=[{command:cycle.name,stage:'ack',status:'client_timeout',deadlineSeconds:.18}];`, `assert.ok(descend($('failure-panel'),'p').some(n=>n.textContent.includes('Previous harness canceled at 180ms; no completion observed')));assert.ok(descend($('timeline'),'rect').some(n=>n.attrs.stroke==='#a32b24'));`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := strings.TrimSuffix(strings.TrimPrefix(htmlTail, "</script><script>"), "</script></body></html>")
			cmd := exec.Command(node)
			cmd.Stdin = strings.NewReader(mockDOM + fixture + tc.setup + "\ndocument.getElementById('report-data').textContent=JSON.stringify(fixtureData);\n" + script + tc.checks)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("latency rendering: %v\n%s", err, output)
			}
		})
	}
}
