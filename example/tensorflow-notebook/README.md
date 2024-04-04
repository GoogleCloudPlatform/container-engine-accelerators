# Jupyter Notebook Scientific Python Stack + Tensorflow with GPU

## What it Gives You

* Tensorflow for Python 2.7 and 3.5 (with GPU support)

## Basic Use

The following command launches an instance of this notebook in a kubernetes cluster with stateful storage.

```
kubectl apply -f tensorflow-notebook.yaml
```

This creates a public service (L4 Loadbalancer) using which you can access your notebook.
Note: The notebook image is large and may take sometime to get running.

Take note of the authentication token included in the notebook startup log messages. Include it in the URL you visit to access the Notebook server or enter it in the Notebook login form.


## Tensorflow Single Machine Mode

```
import tensorflow as tf

hello = tf.Variable('Hello World!')

sess = tf.Session()
init = tf.global_variables_initializer()

sess.run(init)
sess.run(hello)
```

```

