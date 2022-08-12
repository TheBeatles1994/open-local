# Hollow node 大规模环境下存储调度性能测试方案

当在集群创建完毕 hollow node 后，open-local controller 会为每个 hollow node 创建一个 nls。不过由于 hollow node 是虚拟节点，并没有对应的 open-local agent 更新 nls 资源，创建 pod 时 open-local scheduler extender 会自动过滤这些虚拟节点。为了模拟大规模集群的存储调度，本文提出一种模拟真实 nls 数据的方案。

先获取真实节点的 nls 资源（以 json 格式），删除一些多余字段，如下所示。

```json
{
    "apiVersion": "csi.aliyun.com/v1alpha1",
    "kind": "NodeLocalStorage",
    "metadata": {
        "name": "iz0jlcxehy7rn8lkcukz9sz"
    },
    "spec": {
        "listConfig": {
            "devices": {},
            "mountPoints": {},
            "vgs": {
                "include": [
                    "yoda-pool-[0-9]+",
                    "yoda-pool[0-9]+",
                    "open-local-pool-[0-9]+",
                    "ackdistro-pool"
                ]
            }
        },
        "nodeName": "iz0jlcxehy7rn8lkcukz9sz"
    },
    "status": {
        "filteredStorageInfo": {
            "updateStatusInfo": {
                "lastUpdateTime": "2022-08-12T06:47:00Z",
                "updateStatus": "accepted"
            },
            "volumeGroups": [
                "ackdistro-pool"
            ]
        },
        "nodeStorageInfo": {
            "deviceInfo": [
                {
                    "condition": "DiskReady",
                    "mediaType": "hdd",
                    "name": "/dev/vda1",
                    "readOnly": false,
                    "total": 214747299328
                },
                {
                    "condition": "DiskReady",
                    "mediaType": "hdd",
                    "name": "/dev/vda",
                    "readOnly": false,
                    "total": 214748364800
                },
                {
                    "condition": "DiskReady",
                    "mediaType": "hdd",
                    "name": "/dev/vdb",
                    "readOnly": false,
                    "total": 1073741824000
                }
            ],
            "phase": "Running",
            "state": {
                "lastHeartbeatTime": "2022-08-12T06:47:00Z",
                "status": "True",
                "type": "DiskReady"
            },
            "volumeGroups": [
                {
                    "allocatable": 644240900096,
                    "available": 644240900096,
                    "condition": "DiskReady",
                    "logicalVolumes": [
                        {
                            "condition": "DiskReady",
                            "name": "container",
                            "total": 214748364800,
                            "vgname": "ackdistro-pool"
                        },
                        {
                            "condition": "DiskReady",
                            "name": "kubelet",
                            "total": 214748364800,
                            "vgname": "ackdistro-pool"
                        }
                    ],
                    "name": "ackdistro-pool",
                    "physicalVolumes": [
                        "/dev/vdb"
                    ],
                    "total": 1073737629696
                }
            ]
        }
    }
}
```

获取 hollow node 列表：

kubectl get no|grep hollow-node

发现通过 kubectl 命令无法更改 nls 的 status，社区到 [1.24](https://github.com/kubernetes/kubectl/issues/564) 才支持.
可能的命令：
cat hollow-node-nls.json |jq '.metadata.name="hollow-node-999" | .spec.nodeName="hollow-node-999"'|/root/kubectl replace -f --subresource=status  -