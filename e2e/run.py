#!/usr/bin/env python3
import argparse
import atexit
import json
import os
import re
import subprocess
import time


def main(delete_cluster=None,
         create_cluster=None,
         cluster_name=None,
         docker_build=None,
         kind_config=None,
         kind_image=None,
         kubeconfig=None,
         startup_delay=None):

    def kubectl(*args, return_output=False, json_output=False):
        args = ['kubectl', *args]
        env = {**os.environ, 'KUBECONFIG': kubeconfig}
        if json_output:
            args.extend(['-o', 'json'])
            output = subprocess.check_output(args, env=env)
            return json.loads(output)
        elif return_output:
            return subprocess.check_output(args, env=env).decode('utf-8')

        return subprocess.check_call(args, env=env)

    def kind(*args):
        args = ['kind', *args, '--name', cluster_name]
        env = {**os.environ, 'KUBECONFIG': kubeconfig}
        subprocess.check_call(args, env=env)

    def atexit_delete_cluster():
        try:
            kind('delete', 'cluster')
        except subprocess.CalledProcessError as error:
            print('Failed to delete cluster', error)

    #
    # Setup environment for e2e
    #
    if delete_cluster:
        atexit.register(atexit_delete_cluster)

    if create_cluster:
        kind('create', 'cluster',
             '--verbosity', '4',
             '--config', kind_config,
             '--image', kind_image)

    if docker_build:
        subprocess.check_call([
            'docker', 'build',
            '-t', 'mantalabs/validator-elector:e2e', '.'])
        kind('load', 'docker-image', 'mantalabs/validator-elector:e2e')

    kubectl('apply', '-f', 'e2e/rbac.yaml')
    kubectl('apply', '-f', 'e2e/statefulset-tests.yaml')

    # Give things some time to start.
    print(f'\n\nWaiting {startup_delay} seconds ...')
    time.sleep(startup_delay)

    #
    # Make some (kind of lame) assertions
    #
    try:
        # Ensure things are running.
        validator_statefulset = kubectl('get', 'statefulset/validator', '-o', 'json', json_output=True)
        validator_replicas = validator_statefulset['status']['replicas']
        validator_ready_replicas = validator_statefulset['status']['readyReplicas']
        assert validator_replicas == validator_ready_replicas

        # Search for a log message that indicates the elector is communicating with the validator.
        elector_log = kubectl('logs', 'validator-0', 'elector', return_output=True)
        assert re.search('Block .+ is from', elector_log)

    except AssertionError as error:
        print('\n\nAssertion failed!\n\n', error)
        kubectl('describe', 'statefulset/validator')
        raise error

    print('\nSuccess!\n')


def parse_args():
    parser = argparse.ArgumentParser('Run an e2e test')
    parser.add_argument('--kind-config',
                        default=os.path.join(os.path.dirname(os.path.realpath(__file__)), 'kind.yaml'))
    parser.add_argument('--kubeconfig',
                        default=os.path.join(os.path.dirname(os.path.realpath(__file__)), '.kubeconfig'))
    parser.add_argument('--kind-image',
                        default='kindest/node:v1.16.15')
    parser.add_argument('--delete-cluster',
                        dest='delete_cluster',
                        action='store_true')
    parser.add_argument('--no-delete-cluster',
                        dest='delete_cluster',
                        action='store_false')
    parser.add_argument('--create-cluster',
                        dest='create_cluster',
                        action='store_true')
    parser.add_argument('--no-create-cluster',
                        dest='create_cluster',
                        action='store_false')
    parser.add_argument('--cluster-name',
                        default='validator-elector-cluster')
    parser.add_argument('--docker-build',
                        dest='docker_build',
                        action='store_true')
    parser.add_argument('--no-docker-build',
                        dest='docker_build',
                        action='store_false')
    parser.add_argument('--startup-delay',
                        default=30,
                        type=float)

    parser.set_defaults(
        delete_cluster=True,
        create_cluster=True,
        docker_build=True,
    )

    return parser.parse_args()


if __name__ == '__main__':
    main(**vars(parse_args()))
