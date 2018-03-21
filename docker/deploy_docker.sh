#!/bin/bash

usage() {
    echo "Usage: $0 [-e dev|prod|k8s] -a <aws.env> [-s] [-o centos|ubuntu]"
    echo "   -e : dev|prod - deployment environment (default is dev[elopment])"
    echo "   -a : aws.env - AWS credentials"
    echo "   -o : centos|ubuntu - docker base image OS (default ubuntu)"
    echo "   -s : scale the targets: add new targets to an already running cluster, or stop a given number of already running"
    echo
    exit 1;
}
environment="dev";
aws_env="";
os="ubuntu"
while getopts "e:a:o:s:" OPTION
do
    case $OPTION in
    e)
        environment=${OPTARG}
        ;;
    a)
        aws_env=${OPTARG}
        ;;

    o)
        os=${OPTARG}
        ;;

    s)
        scale=${OPTARG}
        ;;
    *)
        usage
        ;;
    esac
done

DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
if [ "${PWD##*/}" != "docker" ]; then
    cd $DIR
fi


if [ ! -z "$scale" ]; then
   echo "Scaling cluster "
   cp /tmp/dfc_backup/* .
   sudo docker-compose -f $environment"_docker-compose.yml" up --no-recreate --scale dfctarget=$scale -d dfctarget
   rm aws.env
   rm dfc.json
   rm Dockerfile
   sudo docker ps
   echo Done
   exit 0
fi

if [ -z "$aws_env" ]; then
   echo -a is a required parameter.Provide the path for aws.env file
   usage
fi

#Copy .deb or .rpm docker file as Dockerfile. This is done keep docker_compose agnostic of docker OS
if [[ "$environment" != "k8s" ]]; then
  if [ $os == "ubuntu" ]; then
     echo "Using debian packaging for the docker container"
     cp Dockerfile.deb Dockerfile
  else
     echo "Usind rpm packaging for the docker container"
     cp Dockerfile.rpm Dockerfile
  fi
fi

PROXYURL="http://dfcproxy:8080"
PORT=8080
SERVICENAME="dfc"
LOGDIR="/tmp/dfc/log"
LOGLEVEL="3"
###################################
#
# fspaths config is used if and only if test_fspaths.count == 0
# existence of each fspath is checked at runtime
#
###################################
TESTFSPATHROOT="/tmp/dfc/"

echo Enter number of cache servers:
read servcount
if ! [[ "$servcount" =~ ^[0-9]+$ ]] ; then
  echo "Error: '$servcount' is not a number"; exit 1
fi
START=0
END=$servcount


echo "Number of local cache directories (enter 0 to use preconfigured filesystems):"
read testfspathcnt
if ! [[ "$testfspathcnt" =~ ^[0-9]+$ ]] ; then
    echo "Error: '$testfspathcnt' is not a number"; exit 1
fi

TESTFSPATHCOUNT=0
FSPATH1="/usr/nvidia/data/1"
FSPATH2="/usr/nvidia/data/2"
FSPATH3="/usr/nvidia/data/3"
FSPATH4="/usr/nvidia/data/4"
FSPATH5="/usr/nvidia/data/5"
FSPATH6="/usr/nvidia/data/6"
FSPATH7="/usr/nvidia/data/7"
FSPATH8="/usr/nvidia/data/8"

echo Select Cloud Provider:
echo  1: Amazon Cloud
echo  2: Google Cloud
echo Enter your choice:
read cldprovider
if [ $cldprovider -eq 1 ]
then
  CLDPROVIDER="aws"
  cp $aws_env .
  if [ "$environment" == "k8s" ]; then
    # creating aws credential files
    rm -rf credentials
    cat $aws_env >> credentials
    sed -i '1 i\[default]' credentials
    sed -i 's/AWS_ACCESS_KEY_ID/aws_access_key_id/g' credentials
    sed -i 's/AWS_SECRET_ACCESS_KEY/aws_secret_access_key/g' credentials
    sed -i 's/AWS_DEFAULT_REGION/region/g' credentials
    kubectl delete secret generic aws-credentials
    kubectl create secret generic aws-credentials --from-file=./credentials
  fi
elif [ $cldprovider -eq 2 ]
then
  CLDPROVIDER="gcp"
else
  echo "Error: '$cldprovider' is not a valid input, can be either 1 or 2"; exit 1
fi

CONFFILE="dfc.json"
c=0
source $DIR/../dfc/setup/config.sh

#1) create/update/delete kubctl configmap
#)  run the cluster

if [ "$environment" == "k8s" ]; then
    # Deploying kubernetes cluster
    echo Starting kubernetes deployment ..
    #Create DFC configmap to attach during runtime
    echo Creating DFC configMap
    kubectl delete configmap dfc-config
    kubectl create configmap dfc-config --from-file=dfc.json

    echo Stopping DFC cluster
    kubectl delete -f dfctarget_deployment.yml
    kubectl delete -f dfcproxy_deployment.yml

    echo Starting Proxy Deployment
    kubectl create -f dfcproxy_deployment.yml

    echo Wating for proxy to start ....
    sleep 100

    echo Starting Target Deployment
    kubectl create -f dfctarget_deployment.yml

    echo Scaling targets
    kubectl scale --replicas=$servcount -f dfctarget_deployment.yml

    echo List of running pods
    kubectl get pods -o wide

else
    echo Stoping running  cluster..
    sudo docker-compose -f $environment"_docker-compose.yml" down
    echo Building Image..
    sudo docker-compose -f $environment"_docker-compose.yml" build
    echo Starting cluster ..
    sudo docker-compose -f $environment"_docker-compose.yml" up -d --scale dfctarget=$servcount
    sleep 3
    sudo docker ps
    echo "Cleaning up files.."
    mkdir -p /tmp/dfc_backup
    mv aws.env /tmp/dfc_backup/
    mv dfc.json /tmp/dfc_backup/
    mv Dockerfile /tmp/dfc_backup/
fi
echo done

