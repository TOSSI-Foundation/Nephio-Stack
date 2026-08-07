/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// mongoExec runs a mongosh --eval against the SD-Core webconsole DB ("aether") by exec-ing into the
// mongodb-0 pod (the RS primary), the same mechanism the old sdcore-provisioner bash used. This is how
// the NetworkSlice controller works around a webconsole bug (below) — a controller reconciling the
// datastore is still the pure-Nephio pattern: declarative intent in, controller actuates.
func mongoExec(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, js string) error {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name("mongodb-0").Namespace("default").SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: "mongodb",
			Command:   []string{"mongosh", "aether", "--quiet", "--eval", js},
			Stdout:    true, Stderr: true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("build mongo exec: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return fmt.Errorf("mongo exec (%s): %w", stderr.String(), err)
	}
	return nil
}

// ensureDnnConfig restores the device-group's ip-domains (DNN + UE pool + QoS) directly in Mongo. The
// SD-Core webconsole DROPS this on the /config/v1/device-group POST, so the SMF never learns the
// SNSSAI+DNN->UPF mapping and rejects every PDU session with "not matched DNN Config". This write makes
// the SMF match the DNN so the PDU session (and UE IP) succeeds. Idempotent ($set).
func ensureDnnConfig(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, dg, dnn, pool string, mbrUp, mbrDn int64) error {
	js := fmt.Sprintf(`db.getCollection('webconsoleData.snapshots.devGroupData').updateOne({'group-name':'%s'},{$set:{'ip-domains':[{'dnn':'%s','ue-ip-pool':'%s','dns-primary':'8.8.8.8','mtu':1400,'ue-dnn-qos':{'dnn-mbr-downlink':%d,'bitrate-unit':'bps','traffic-class':{'qci':9,'arp':1,'pdb':300,'pelr':6,'name':'platinum'},'dnn-mbr-uplink':%d}}]}})`,
		dg, dnn, pool, mbrDn, mbrUp)
	return mongoExec(ctx, cfg, cs, js)
}

// ensureProvisionedData clones the per-subscriber provisioned data (auth/session/policy) for an IMSI if
// the webconsole didn't create it (another webconsole gap for slices beyond the seed), by templating an
// existing subscriber's doc with this IMSI + the slice SD. Idempotent (only inserts when count==0).
func ensureProvisionedData(ctx context.Context, cfg *rest.Config, cs *kubernetes.Clientset, imsi, sd string) error {
	for _, c := range []string{
		"subscriptionData.provisionedData.amData",
		"subscriptionData.provisionedData.smData",
		"subscriptionData.provisionedData.smfSelectionSubscriptionData",
		"policyData.ues.amData",
		"policyData.ues.smData",
	} {
		// Target the SD field PRECISELY: replace the `"sd":"<6hex>"` value(s), NOT the first 6-hex run anywhere
		// in the serialized doc (which would clobber an ObjectId fragment / key / auth value -> silent corruption).
		js := fmt.Sprintf(`var c=db.getCollection('%s'); if(c.countDocuments({ueId:/%s/})==0){var t=c.findOne({ueId:{$exists:true}}); if(t){var o=JSON.parse(JSON.stringify(t).replace(/"sd":"[0-9a-fA-F]{6}"/g,'"sd":"%s"').replace(new RegExp(t.ueId.replace('imsi-',''),'g'),'%s')); o.ueId='imsi-%s'; delete o._id; c.insertOne(o);}}`,
			c, imsi, sd, imsi, imsi)
		if err := mongoExec(ctx, cfg, cs, js); err != nil {
			return fmt.Errorf("clone %s for %s: %w", c, imsi, err)
		}
	}
	return nil
}
