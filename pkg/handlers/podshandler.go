package handlers

import (
	"bytes"
	"context"
	"cuelang.org/go/cue"
	"cuelang.org/go/tools/flow"
	"fmt"
	"io"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/resource"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"os"
	"os/exec"
	"strings"
)

const (
	PodFlowTpl  = "pkg/flowtpls/workflow.cue"
	PodFlowRoot = "workflow"
)

func init() {
	f := NewFlowFunc(PodFlowTpl, PodFlowRoot, workflowHandler)
	Register("workflow", "多个POD工作流操作", f)
}

func getPodLogs(obj *resource.Info) string {
	podLogOpts := &v1.PodLogOptions{}
	req := obj.Client.Get().Namespace(obj.Namespace).
		Name(obj.Name).
		Resource(obj.ResourceMapping().Resource.Resource).
		SubResource("log").
		VersionedParams(podLogOpts, scheme.ParameterCodec)
	podLogs, err := req.Stream(context.Background())
	if err != nil {
		return err.Error()
	}
	defer podLogs.Close()

	buf := new(bytes.Buffer)
	_, err = io.Copy(buf, podLogs)
	if err != nil {
		return err.Error()
	}
	str := buf.String()

	return str
}

// waitForStatusByInformer 使用informer监听等待pod状态
func waitForStatusByInformer(obj *resource.Info) error {
	var err error
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("%v", e)
		}
	}()

	lw := cache.NewListWatchFromClient(obj.Client, obj.ResourceMapping().Resource.Resource, obj.Namespace, fields.Everything())
	informer := cache.NewSharedIndexInformer(lw, obj.Object, 0, nil)
	ch := make(chan struct{})
	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			pod := &v1.Pod{}
			err := runtime.DefaultUnstructuredConverter.FromUnstructured(newObj.(*unstructured.Unstructured).UnstructuredContent(), &pod)
			if err != nil {
				klog.Errorf("pod name [%s] namespace [%s], informer error", pod.Name, pod.Namespace)
				close(ch)
				return
			}

			// 当此pod是running时，关闭informer
			if pod.Status.Phase == v1.PodRunning {
				klog.Infof("pod name [%s] namespace [%s], success", pod.Name, pod.Namespace)
				close(ch)
			}

		},
	})
	// 阻塞运行，直到informer停止
	informer.Run(ch)
	return err
}

func workflowHandler(v cue.Value) (flow.Runner, error) {
	l, b := v.Label()
	// 如果是根节点，跳过
	if !b || l == PodFlowRoot {
		return nil, nil
	}
	return flow.RunnerFunc(func(t *flow.Task) error {

		if t.Index() != 0 {

			action := getField(t.Value(), "action", "apply")

			taskType := getField(t.Value(), "type", "k8s")

			// 执行k8s流程
			if taskType == "k8s" {
				podJson, err := jsonField(t.Value(), "template")
				if err != nil {
					return err
				}
				// 区分两种动作 apply delete
				if action == "apply" {
					res, err := apply(podJson)
					if err != nil {
						return err
					}
					// TODO: 如果需要支持其他资源对象，需要修改informer
					// 如果返回的pod对象有多个，则调用waitForStatusByInformer
					if len(res) > 0 {
						err = waitForStatusByInformer(res[0])
						return err
					}
				} else {
					err = delete(podJson)
					if err != nil {
						return err
					}
				}
			}

			// 执行脚本流程
			if taskType == "bash" {

				scriptToRun := getField(t.Value(), "script", "")
				// 创建命令对象
				cmd := exec.Command("bash", "-s")
				cmd.Stdin = strings.NewReader(scriptToRun)
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr

				// 执行脚本
				err := cmd.Start()
				if err != nil {
					fmt.Println("启动脚本时出错:", err)
					return err
				}

				// 等待脚本执行完成
				err = cmd.Wait()
				if err != nil {
					fmt.Println("执行脚本时出错:", err)
					return err
				}
			}
		}

		return nil
	}), nil
}
