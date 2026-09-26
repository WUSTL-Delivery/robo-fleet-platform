from setuptools import find_packages, setup

package_name = 'fleet_agent'

setup(
    name=package_name,
    version='0.0.1',
    packages=find_packages(exclude=['test']),
    data_files=[
        ('share/ament_index/resource_index/packages',
            ['resource/' + package_name]),
        ('share/' + package_name, ['package.xml']),
        ('share/' + package_name + '/launch', ['launch/fleet_agent.launch.py']),
        ('share/' + package_name + '/config', [
            'config/fleet_agent.yaml',
            'config/twist_mux_fleet.yaml',
        ]),
    ],
    # `fleet` (sdk/python) is not published yet, so it is not listed here: installing it
    # would send pip to PyPI for a name that is not there. See the README.
    install_requires=['setuptools'],
    zip_safe=True,
    maintainer='Jonathan Rodriguez-Gomez',
    maintainer_email='j.rodriguezgomez@wustl.edu',
    description='Bridges a ROS 2 robot onto fleet-server: manifest, twist, telemetry, help.',
    license='Apache-2.0',
    extras_require={
        'test': [
            'pytest',
        ],
    },
    entry_points={
        'console_scripts': [
            'fleet_agent = fleet_agent.agent_node:main',
        ],
    },
)
