import sys
import pandas as pd
import json
import os


data = sys.stdin.read()  
data = json.loads(data)

df = pd.json_normalize(data)

output = os.environ['OUTPUT_FOLDER']
file_name = '{}/result.csv'.format(output)
df.to_csv(file_name, index=False)
