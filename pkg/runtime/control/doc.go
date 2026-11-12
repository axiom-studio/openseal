// Package control holds the control tools an agent calls instead of a
// Skill action: ask user, wait, react, report progress, start task, fork,
// delegate and reply, run operation, request decision, completion review
// and the final answer with its checks. ControlExecutor runs them inline or
// at settle and resumes their parked entries; the control authority decides
// which tools a Run is offered, and the loop interaction guards (live
// browser rate limits, pending setup forms) read the Run's governed calls.
package control
