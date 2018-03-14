#!/bin/bash

#test:disabled
# we may not need this to run as a pre-check-in test for every PR. but only once in a while to ensure nothing's broken.

set -euo pipefail

id=""

cleanup() {
    # TODO : Cleanup objs in the end and verify pods are recycled by corresponding mgr processes in the end
    log "WIP"
}

create_python_source_code() {
    mkdir testDir
    printf 'def main():\n    return "Hello, world!"' > testDir/hello.py
}


verify_function_pod_ns() {
    function_label=$1
    env_ns=$2

    kubectl get pods -n $env_ns -L functionName| grep $function_label
}


verify_obj_in_ns() {
    obj_kind=$1
    obj_name=$2
    obj_ns=$3

    kubectl get $obj_kind $obj_name -n $obj_ns
}

get_pkg_name_from_func() {
    pkg=`kubectl get function $1 -n $2 -o jsonpath='{.spec.package.packageref.name}'`
    echo "$pkg"
}

# since we havent deleted the funcs created previously in new_deploy_mgr tests,
# just curl on the funcs and verifying http response code should be enough
internal_route_test_1() {
    http_status=`curl -sw "%{http_code}" http://FISSION_ROUTER/$FISSION_NAMESPACE/func5 -o /tmp/file`
    [[ "$http_status" -ne "200" ]]; log "internal route test for http://FISSION_ROUTER/func5 returned http_status: $http_status" && exit 1
}

internal_route_test_2() {
    ns="ns2-$id"
    http_status=`curl -sw "%{http_code}" http://FISSION_ROUTER/$FISSION_NAMESPACE/$ns/func4 -o /tmp/file`
    [[ "$http_status" -ne "200" ]]; log "internal route test for http://FISSION_ROUTER/$ns/func4 returned http_status: $http_status" && exit 1
}

new_deploy_mgr_test_2() {
    fission env create --name python --image fission/python-env --envns "ns1-$id"
    fission func create --name func5 --env python --envns "ns1-$id" --code testDir/hello.py --minscale 1 --maxscale 4 --executortype newdeploy
    sleep 5

    # function is loaded in $FISSION_NAMESPACE because func object was created in default ns
    verify_function_pod_ns func5 $FISSION_NAMESPACE || (log "func func4 not specialized in ns2-$id" && exit 1)
}

new_deploy_mgr_test_1() {
    fission env create --name python --image fission/python-env --envns "ns1-$id"
    fission func create --name func4 --fns "ns2-$id" --env python --envns "ns1-$id" --code testDir/hello.py --minscale 1 --maxscale 4 --executortype newdeploy
    sleep 5

    # note that this test is diff from pool_mgr test because here function is loaded in func ns and not in env ns
    verify_function_pod_ns func4 "ns2-$id" || (log "func func4 not specialized in ns2-$id" && exit 1)
}

builder_mgr_test_2() {
    fission env create --name python-builder-env --builder fission/python-builder --image fission/python-env
    zip -jr src-pkg.zip $ROOT/examples/python/sourcepkg/
    pkg=$(fission package create --src src-pkg-2.zip --env python-builder --buildcmd "./build.sh" --pkgns "ns2-$id"| cut -f2 -d' '| tr -d \')
    timeout 60s bash -c "waitBuild $pkg"
    fission fn create --name func4 --fns "ns2-$id" --pkg $pkg --pkgns "ns2-$id" --entrypoint "user.main"
    fission route create --function func4 --fns "ns2-$id" --url /func4 --method GET

    # get the function loaded into a pod
    curl http://FISSION_ROUTER/func4
    sleep 3

    # verify the function specialized pod is in fission-builder. This also verifies builder ran successfully and in fission-builder
    verify_function_pod_ns func4 fission-builder || (log "func func4 not specialized in fission-builder" && exit 1)
}

builder_mgr_test_1() {
    fission env create --name python-builder-env --envns "ns1-$id" --builder fission/python-builder --image fission/python-env
    zip -jr src-pkg.zip $ROOT/examples/python/sourcepkg/
    pkg=$(fission package create --src src-pkg.zip --env python-builder --envns "ns1-$id" --buildcmd "./build.sh" --pkgns "ns2-$id"| cut -f2 -d' '| tr -d \')
    timeout 60s bash -c "waitBuild $pkg"
    fission fn create --name func3 --fns "ns2-$id" --pkg $pkg --pkgns "ns2-$id" --entrypoint "user.main"
    fission route create --function func3 --fns "ns2-$id" --url /func3 --method GET

    # get the function loaded into a pod
    curl http://FISSION_ROUTER/func3
    sleep 3

    # verify the function specialized pod is in ns1-$id. This also verifies builder ran successfully and in ns1-$id
    verify_function_pod_ns func3 "ns1-$id" || (log "func func3 not specialized in ns1-$id" && exit 1)
}

pool_mgr_test_2() {
    fission env create --name python --image fission/python-env
    fission func create --name func2 --fns "ns2-$id" --env python --code testDir/hello.py
    fission route create --function func2 --fns "ns2-$id" --url /func2

    # verify that env object is created in default ns when envns option is absent with fission env create command
    verify_obj_in_ns environment python default|| (log "env python not found in default ns" && exit 1)

    # get the function loaded into a pod
    curl http://FISSION_ROUTER/func2
    sleep 3

    # note that the env pod is created in the $FISSION_FUNCTION ns though the env object is created in default ns
    # so even if the function is created in a different ns, they will be loaded in the $FISSION_FUNCTION.
    verify_function_pod_ns func2 $FISSION_FUNCTION || (log "func func2 not specialized in $FISSION_FUNCTION" && exit 1)
}

pool_mgr_test_1() {
    fission env create --name python --image fission/python-env --envns "ns1-$id"
    fission func create --name func1 --fns "ns2-$id" --env python --envns "ns1-$id" --code testDir/hello.py
    ht=$(fission route create --function func1 --fns "ns2-$id" --url /func1 | cut -f2 -d' '| tr -d \')

    # verify that fission objects are created in the expected namespaces
    verify_obj_in_ns environment python "ns1-$id" || (log "env python not found in ns1-$id" && exit 1)
    verify_obj_in_ns "function" func1 "ns2-$id" || (log "function func1 not found in ns2-$id" && exit 1)
    pkg=$( get_pkg_name_from_func func1 "ns2-$id" )
    verify_obj_in_ns package $pkg "ns2-$id" || (log "package $pkg not found in ns2-$id" && exit 1)
    verify_obj_in_ns httptrigger $ht "ns2-$id" || (log "http trigger $ht not found in ns2-$id" && exit 1)

    # get the function loaded into a pod
    curl http://FISSION_ROUTER/func1
    sleep 3

    # note that the env pod is created in the ns that env is created
    # so even if the function is created in a different ns, they will be loaded in the env pods ns.
    # this behavior is for poolmgr so that functions can utilize the resources better.
    verify_function_pod_ns func1 "ns1-$id" || (log "func func1 not specialized in ns1-$id" && exit 1)
}


main() {
    # extract the test-id generated for this CI test run, so that they can be suffixed to namespaces created as part of
    # this test and namespaces wont clash when fission CI tests are run in parallel in the future.
    id=`echo $FISSION_NAMESPACE| cut -d"-" -f2`
    echo "test_id : $id"

    # pool mgr tests
    # 1. env ns1, func ns2 with code, route and verify specialized pod in ns1, also verify pkg in ns2
    pool_mgr_test_1

    # 3. env default, func with code in ns2, route and verify specialized pod in $FUNCTION_NAMESPACE.
    # this test is to verify backward compatibility
    pool_mgr_test_2


    # builder mgr tests
    # 1. env with builder image and runtime image in ns1, src pkg and func in ns2, route and verify specialized pod in ns1
    builder_mgr_test_1

    # 2. env with builder image and runtime image in default, src pkg and func in ns2,
    #    route and verify specialized pod in fission-builder. this test is to verify backward compatibility
    builder_mgr_test_2


    # new deploy mgr tests
    # 1. env ns1, func ns2 with code, route and verify specialized pod in ns2,
    new_deploy_mgr_test_1

    # 2. env default, func ns2 with code, route and verify specialized pod in fission-function
    new_deploy_mgr_test_2

    # internal route tests.
    # 1. env ns1, func ns2 with code, curl http://FISSION_ROUTER/fission-function/ns2/func -> should work
    internal_route_test_1

    # 2. env in default, func ns2 with code, curl http://FISSION_ROUTER/fission-function/func -> should work
    internal_route_test_2

    # TODO : I may not need to add below tests because testing internal route essentially is the same.
    # Need to verify from Soam

    # timer trigger tests.
    # 1. env ns1, func ns2 with code, tt ( with a one time cron string for executing imm'ly), verify function is executed
    # this also indirectly tests internal route established at http://FISSION_ROUTER/ns1/func

    # 2. env in default, func ns2 with code, tt ( with a one time cron string for executing imm'ly), verify function is executed
    # this also indirectly tests internal route established at http://FISSION_ROUTER/func


    # kube watch tests.
    # 1. env ns1, func ns2 with code, watch Trigger ( TBD), verify function is executed
    # this also indirectly tests internal route established at http://FISSION_ROUTER/ns1/func

    # 2. env in default, func ns2 with code, watch Trigger ( TBD ), verify function is executed
    # this also indirectly tests internal route established at http://FISSION_ROUTER/func


    # msq trigger tests.
    # integrate after mqtrigger tests are checked into master.

    cleanup
}

main