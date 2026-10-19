import time
import os
from kubernetes import client, config
from google.cloud import monitoring_v3
import google.auth
from googleapiclient.discovery import build

def query_cloud_monitoring_for_status(project_id, instance_id, monitoring_client):
    """Queries Cloud Monitoring API for the GPU failure prediction status from metric labels."""
    try:
        project_name = f"projects/{project_id}"
        now = time.time()
        # Look back 15 minutes to ensure we catch a recent point
        start_time = int(now - 900)
        end_time = int(now)
        interval = monitoring_v3.TimeInterval(
            start_time={"seconds": start_time},
            end_time={"seconds": end_time}
        )
        filter_str = (
            f'metric.type="compute.googleapis.com/instance/gpu/failure_prediction_status" '
            f'AND resource.type="gce_instance" '
            f'AND resource.labels.instance_id="{instance_id}"'
        )

        results = monitoring_client.list_time_series(
            request={
                "name": project_name,
                "filter": filter_str,
                "interval": interval,
                "view": monitoring_v3.ListTimeSeriesRequest.TimeSeriesView.HEADERS,
            }
        )
        print(f"Results: {results}")
        found_status = None
        for series in results:
            metric_labels = series.metric.labels
            prediction_value = metric_labels.get("Value")

            if prediction_value:
                print(f"  Instance {instance_id}: Found metric label 'Value': {prediction_value}")
                if prediction_value in ["NO_DEGRADATION_PREDICTED", "POSSIBLE_DEGRADATION_PREDICTED"]:
                    found_status = "True"
                elif prediction_value == "DEGRADATION_PREDICTED":
                    found_status = "False"
                else:
                    print(f"  WARNING: Unknown prediction value: {prediction_value}")
                    found_status = "UNKNOWN"
                return found_status
            else:
                print(f"  WARNING: Metric label 'Value' not found in series for instance {instance_id}")

        if not found_status:
             print(f"  No series with 'Value' metric label found for instance {instance_id} in the interval.")
        return None

    except Exception as e:
        print(f"Error querying Cloud Monitoring for instance {instance_id}: {e}")
        return None

def get_instance_id(compute_service, project_id, zone, instance_name):
    """Fetches the numeric GCE instance ID."""
    try:
        result = compute_service.instances().get(
            project=project_id,
            zone=zone,
            instance=instance_name).execute()
        return result.get('id')
    except Exception as e:
        print(f"Error getting instance details for {instance_name} in {zone}: {e}")
        return None

def update_all_node_labels(kube, monitoring_client, compute_service, project_id):
    """Fetches status and updates labels for all relevant nodes."""
    print("Listing nodes with GPUs...")
    try:
        nodes = kube.list_node(label_selector="cloud.google.com/gke-gpu=true")
    except client.exceptions.ApiException as e:
        print(f"Error listing nodes: {e}")
        return

    print(f"Found {len(nodes.items)} GPU nodes to process.")
    for node in nodes.items:
        node_name = node.metadata.name
        provider_id = node.spec.provider_id
        zone = node.metadata.labels.get("topology.kubernetes.io/zone")

        if not provider_id or not provider_id.startswith("gce://"):
            print(f"Node {node_name} has non-GCE or missing providerID: {provider_id}. Skipping.")
            continue

        try:
            instance_name = provider_id.split('/')[-1]
        except Exception as e:
            print(f"Could not parse instance name from providerID {provider_id} for node {node_name}: {e}")
            continue

        if not zone:
            print(f"Node {node_name} is missing 'topology.kubernetes.io/zone' label. Skipping.")
            continue

        print(f"Processing Node: {node_name}, Instance Name: {instance_name}, Zone: {zone}")

        instance_id = get_instance_id(compute_service, project_id, zone, instance_name)

        if not instance_id:
            print(f"  Failed to get GCE Instance ID for node {node_name}. Skipping.")
            continue

        print(f"  GCE Instance ID: {instance_id}")

        status = query_cloud_monitoring_for_status(project_id, instance_id, monitoring_client)

        if status is not None:
            if status == "UNKNOWN":
                 print(f"  Label not updated for {node_name} due to UNKNOWN prediction value.")
            else:
                label_value = status
                node_labels = {
                    "gke.io/recommended-to-run-large-training-workload": label_value
                }
                try:
                    kube.patch_node(node_name, {"metadata": {"labels": node_labels}})
                    print(f"  Successfully updated labels on node {node_name}: {node_labels}")
                except client.exceptions.ApiException as e:
                    print(f"  Error patching node {node_name}: {e}")
        else:
            print(f"  Could not retrieve status for node {node_name} (Instance ID: {instance_id}). Labels not updated.")

if __name__ == "__main__":
    print("Starting label-nodes cronjob...")

    credentials, detected_project_id = google.auth.default(scopes=[
        'https://www.googleapis.com/auth/monitoring.read',
        'https://www.googleapis.com/auth/compute.readonly'
    ])
    project_id = detected_project_id

    if not project_id:
        print("ERROR: Project ID is unknown. Exiting.")
        exit(1)

    print(f"Using Project ID: {project_id}")

    try:
        config.load_incluster_config()
        kube = client.CoreV1Api()
        print("Kubernetes client initialized.")

        # Get credentials for GCP APIs
        credentials, _ = google.auth.default(scopes=[
            'https://www.googleapis.com/auth/monitoring.read',
            'https://www.googleapis.com/auth/compute.readonly'
        ])
        monitoring_client = monitoring_v3.MetricServiceClient(credentials=credentials)
        compute_service = build('compute', 'v1', credentials=credentials)
        print("GCP clients initialized.")

    except Exception as e:
        print(f"Failed to initialize clients: {e}")
        exit(1)

    update_all_node_labels(kube, monitoring_client, compute_service, project_id)

    print("label-nodes cronjob finished.")
