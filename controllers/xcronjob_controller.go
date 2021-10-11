/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/go-logr/logr"
	"github.com/robfig/cron"
	kbatch "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ref "k8s.io/client-go/tools/reference"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batchv1 "xcronjob/api/v1"
)

// XCronJobReconciler reconciles a XCronJob object
type XCronJobReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
	Clock
}

type realClock struct{}

func (_ realClock) Now() time.Time { return time.Now() }

// clock接口可以获取当前的时间
// 可以帮助我们在测试中模拟计时
type Clock interface {
	Now() time.Time
}

//+kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=xcronjobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=xcronjobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch.tutorial.kubebuilder.io,resources=xcronjobs/finalizers,verbs=update

//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=batch,resources=jobs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the XCronJob object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
var (
	scheduledTimeAnnotation = "batch.tutorial.kubebuilder.io/scheduled-at"
)

func (r *XCronJobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("xcronjob", req.NamespacedName)

	// your logic here
	var xcronJob batchv1.XCronJob
	if err := r.Get(ctx, req.NamespacedName, &xcronJob); err != nil {
		log.Error(err, "unable to fetch XCronJob")
		//忽略掉 not-found 错误，它们不能通过重新排队修复（要等待新的通知）
		//在删除一个不存在的对象时，可能会报这个错误。
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	var childJobs kbatch.JobList
	if err := r.List(ctx, &childJobs, client.InNamespace(req.Namespace), client.MatchingFields{jobOwnerKey: req.Name}); err != nil {
		log.Error(err, "unable to list child Jobs")
		return ctrl.Result{}, err
	}

	// 找出所有有效的 job
	var activeJobs []*kbatch.Job
	var successfulJobs []*kbatch.Job
	var failedJobs []*kbatch.Job
	var mostRecentTime *time.Time // 记录其最近一次运行时间以便更新状态

	isJobFinished := func(job *kbatch.Job) (bool, kbatch.JobConditionType) {
		for _, c := range job.Status.Conditions {
			if (c.Type == kbatch.JobComplete || c.Type == kbatch.JobFailed) && c.Status == corev1.ConditionTrue {
				return true, c.Type
			}
		}
		return false, ""
	}

	getScheduledTimeForJob := func(job *kbatch.Job) (*time.Time, error) {
		timeRaw := job.Annotations[scheduledTimeAnnotation]
		if len(timeRaw) == 0 {
			return nil, nil
		}

		timeParsed, err := time.Parse(time.RFC3339, timeRaw)
		if err != nil {
			return nil, err
		}
		return &timeParsed, nil
	}

	for i, job := range childJobs.Items {
		_, finishedType := isJobFinished(&job)
		switch finishedType {
		case "": // ongoing
			activeJobs = append(activeJobs, &childJobs.Items[i])
		case kbatch.JobFailed:
			failedJobs = append(failedJobs, &childJobs.Items[i])
		case kbatch.JobComplete:
			successfulJobs = append(successfulJobs, &childJobs.Items[i])
		}

		//将启动时间存放在注释中，当job生效时可以从中读取
		scheduledTimeForJob, err := getScheduledTimeForJob(&job)
		if err != nil {
			log.Error(err, "unable to parse schedule time for child job", "job", &job)
			continue
		}
		if scheduledTimeForJob != nil {
			if mostRecentTime == nil {
				mostRecentTime = scheduledTimeForJob
			} else if mostRecentTime.Before(*scheduledTimeForJob) {
				mostRecentTime = scheduledTimeForJob
			}
		}
	}

	if mostRecentTime != nil {
		xcronJob.Status.LastScheduleTime = &metav1.Time{Time: *mostRecentTime}
	} else {
		xcronJob.Status.LastScheduleTime = nil
	}
	xcronJob.Status.Active = nil
	for _, activeJob := range activeJobs {
		jobRef, err := ref.GetReference(r.Scheme, activeJob)
		if err != nil {
			log.Error(err, "unable to make reference to active job", "job", activeJob)
			continue
		}
		xcronJob.Status.Active = append(xcronJob.Status.Active, *jobRef)
	}
	log.V(1).Info("job count:", "active jobs", len(activeJobs), "successful jobs", len(successfulJobs), "failed jobs", len(failedJobs))
	if err := r.Status().Update(ctx, &xcronJob); err != nil {
		log.Error(err, "unable to update CronJob status")
		return ctrl.Result{}, err
	}

	// 注意: 删除操作采用的“尽力而为”策略
	// 如果个别 job 删除失败了，不会将其重新排队，直接结束删除操作
	if xcronJob.Spec.FailedJobsHistoryLimit != nil {
		sort.Slice(failedJobs, func(i, j int) bool {
			if failedJobs[i].Status.StartTime == nil {
				return failedJobs[j].Status.StartTime != nil
			}
			return failedJobs[i].Status.StartTime.Before(failedJobs[j].Status.StartTime)
		})
		for i, job := range failedJobs {
			if int32(i) >= int32(len(failedJobs))-*xcronJob.Spec.FailedJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete old failed job", "job", job)
			} else {
				log.V(0).Info("deleted old failed job", "job", job)
			}
		}
	}

	if xcronJob.Spec.SuccessfulJobsHistoryLimit != nil {
		sort.Slice(successfulJobs, func(i, j int) bool {
			if successfulJobs[i].Status.StartTime == nil {
				return successfulJobs[j].Status.StartTime != nil
			}
			return successfulJobs[i].Status.StartTime.Before(successfulJobs[j].Status.StartTime)
		})
		for i, job := range successfulJobs {
			if int32(i) >= int32(len(successfulJobs))-*xcronJob.Spec.SuccessfulJobsHistoryLimit {
				break
			}
			if err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); (err) != nil {
				log.Error(err, "unable to delete old successful job", "job", job)
			} else {
				log.V(0).Info("deleted old successful job", "job", job)
			}
		}
	}

	if xcronJob.Spec.Suspend != nil && *xcronJob.Spec.Suspend {
		log.V(1).Info("xcronjob suspended, skipping")
		return ctrl.Result{}, nil
	}

	getNextSchedule := func(xcronJob *batchv1.XCronJob, now time.Time) (lastMissed time.Time, next time.Time, err error) {
		sched, err := cron.ParseStandard(xcronJob.Spec.Schedule)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("Unparseable schedule %q: %v", xcronJob.Spec.Schedule, err)
		}

		// 出于优化的目的，我们可以使用点技巧。从上一次观察到的执行时间开始执行，
		// 这个执行时间可以被在这里被读取。但是意义不大，因为我们刚更新了这个值。

		var earliestTime time.Time
		if xcronJob.Status.LastScheduleTime != nil {
			earliestTime = xcronJob.Status.LastScheduleTime.Time
		} else {
			earliestTime = xcronJob.ObjectMeta.CreationTimestamp.Time
		}
		if xcronJob.Spec.StartingDeadlineSeconds != nil {
			// 如果开始执行时间超过了截止时间，不再执行
			schedulingDeadline := now.Add(-time.Second * time.Duration(*xcronJob.Spec.StartingDeadlineSeconds))

			if schedulingDeadline.After(earliestTime) {
				earliestTime = schedulingDeadline
			}
		}
		if earliestTime.After(now) {
			return time.Time{}, sched.Next(now), nil
		}

		starts := 0
		for t := sched.Next(earliestTime); !t.After(now); t = sched.Next(t) {
			lastMissed = t
			// 一个 XCronJob 可能会遗漏多次执行。举个例子，周五 5:00pm 技术人员下班后，
			// 控制器在 5:01pm 发生了异常。然后直到周二早上才有技术人员发现问题并
			// 重启控制器。那么所有的以1小时为周期执行的定时任务，在没有技术人员
			// 进一步的干预下，都会有 80 多个 job 在恢复正常后一并启动（如果 job 允许
			// 多并发和延迟启动）

			// 如果 XCronJob 的某些地方出现异常，控制器或 apiservers (用于设置任务创建时间)
			// 的时钟不正确, 那么就有可能出现错过很多次执行时间的情形（跨度可达数十年）
			// 这将会占满控制器的CPU和内存资源。这种情况下，我们不需要列出错过的全部
			// 执行时间。

			starts++
			if starts > 100 {
				// 获取不到最近一次执行时间，直接返回空切片
				return time.Time{}, time.Time{}, fmt.Errorf("Too many missed start times (> 100). Set or decrease .spec.startingDeadlineSeconds or check clock skew.")
			}
		}
		return lastMissed, sched.Next(now), nil
	}
	// 计算出定时任务下一次执行时间（或是遗漏的执行时间）
	missedRun, nextRun, err := getNextSchedule(&xcronJob, r.Now())
	if err != nil {
		log.Error(err, "unable to figure out XCronJob schedule")
		// 重新排队直到有更新修复这次定时任务调度，不必返回错误
		return ctrl.Result{}, nil
	}
	scheduledResult := ctrl.Result{RequeueAfter: nextRun.Sub(r.Now())} // 保存以便别处复用
	log = log.WithValues("now", r.Now(), "next run", nextRun)

	if missedRun.IsZero() {
		log.V(1).Info("no upcoming scheduled times, sleeping until next")
		return scheduledResult, nil
	}
	// 确保错过的执行没有超过截止时间
	log = log.WithValues("current run", missedRun)
	tooLate := false
	if xcronJob.Spec.StartingDeadlineSeconds != nil {
		tooLate = missedRun.Add(time.Duration(*xcronJob.Spec.StartingDeadlineSeconds) * time.Second).Before(r.Now())
	}
	if tooLate {
		log.V(1).Info("missed starting deadline for last run, sleeping till next")
		// TODO(directxman12): events
		return scheduledResult, nil
	}

	// 确定要 job 的执行策略 —— 并发策略可能禁止多个job同时运行
	if xcronJob.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent && len(activeJobs) > 0 {
		log.V(1).Info("concurrency policy blocks concurrent runs, skipping", "num active", len(activeJobs))
		return scheduledResult, nil
	}
	// 直接覆盖现有 job
	if xcronJob.Spec.ConcurrencyPolicy == batchv1.ReplaceConcurrent {
		for _, activeJob := range activeJobs {
			// we don't care if the job was already deleted
			if err := r.Delete(ctx, activeJob, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
				log.Error(err, "unable to delete active job", "job", activeJob)
				return ctrl.Result{}, err
			}
		}
	}

	constructJobForXCronJob := func(xcronJob *batchv1.XCronJob, scheduledTime time.Time) (*kbatch.Job, error) {
		// job 名称带上执行时间以确保唯一性，避免排定执行时间的 job 创建两次
		name := fmt.Sprintf("%s-%d", xcronJob.Name, scheduledTime.Unix())

		job := &kbatch.Job{
			ObjectMeta: metav1.ObjectMeta{
				Labels:      make(map[string]string),
				Annotations: make(map[string]string),
				Name:        name,
				Namespace:   xcronJob.Namespace,
			},
			Spec: *xcronJob.Spec.JobTemplate.Spec.DeepCopy(),
		}
		for k, v := range xcronJob.Spec.JobTemplate.Annotations {
			job.Annotations[k] = v
		}
		job.Annotations[scheduledTimeAnnotation] = scheduledTime.Format(time.RFC3339)
		for k, v := range xcronJob.Spec.JobTemplate.Labels {
			job.Labels[k] = v
		}
		if err := ctrl.SetControllerReference(xcronJob, job, r.Scheme); err != nil {
			return nil, err
		}

		return job, nil
	}

	// 构建 job
	job, err := constructJobForXCronJob(&xcronJob, missedRun)
	if err != nil {
		log.Error(err, "unable to construct job from template")
		// job 的 spec 没有变更，无需重新排队
		return scheduledResult, nil
	}

	// ...在集群中创建 job
	if err := r.Create(ctx, job); err != nil {
		log.Error(err, "unable to create Job for XCronJob", "job", job)
		return ctrl.Result{}, err
	}

	log.V(1).Info("created Job for XCronJob run", "job", job)
	return scheduledResult, nil
}

// SetupWithManager sets up the controller with the Manager.
var (
	jobOwnerKey = ".metadata.controller"
	apiGVStr    = batchv1.GroupVersion.String()
)

func (r *XCronJobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// 此处不是测试，我们需要创建一个真实的时钟
	if r.Clock == nil {
		r.Clock = realClock{}
	}

	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &kbatch.Job{}, jobOwnerKey, func(rawObj client.Object) []string {
		//获取 job 对象，提取 owner...
		job := rawObj.(*kbatch.Job)
		owner := metav1.GetControllerOf(job)
		if owner == nil {
			return nil
		}
		// ...确保 owner 是个 XCronJob...
		if owner.APIVersion != apiGVStr || owner.Kind != "XCronJob" {
			return nil
		}

		// ...是 XCronJob，返回
		return []string{owner.Name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&batchv1.XCronJob{}).
		Owns(&kbatch.Job{}).
		Complete(r)
}
